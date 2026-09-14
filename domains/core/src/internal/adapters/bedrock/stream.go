// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Native response streaming via Bedrock ConverseStream.
//
// WHAT THIS BUYS: without it the adapter returns the complete answer and the handler
// slices it into SSE frames, so first-token time equals last-token time. The transport in
// front (API Gateway response streaming) was the previous bottleneck and is fixed; this is
// the other half, and it is the half that a user actually perceives.
//
// ConverseStream is used rather than InvokeModelWithResponseStream for the same reason the
// buffered path uses Converse: Converse normalizes the model families, so there is one
// translation instead of one per family. The IAM policy already allows it
// (bedrock:InvokeModelWithResponseStream covers ConverseStream).
//
// The request is built by the SAME convertMessages / ConvertToolsToBedrockConfig / cache
// point code as the buffered path. That is deliberate: two translations that must agree
// but are written twice is how a streaming request starts silently differing from a
// buffered one (different system handling, dropped images, ungrouped toolResults).
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

var _ ports.StreamProvider = (*Adapter)(nil) // compile-time assertion

// converseStreamAPI is the narrow slice of the Bedrock client this file needs, so the
// open-and-validate step can be driven without a real client.
type converseStreamAPI interface {
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

// eventStream is the slice of ConverseStreamEventStream that brStream uses.
//
// It is an interface for one reason: the SDK's concrete event stream reads from a live
// HTTP body and cannot be constructed in a test. Behind this interface plus an injected
// channel, the whole event translation — deltas, usage, cache counters, fragmented tool
// input, and the difference between a clean end and a broken stream — is exercisable
// without the network. That difference is exactly the part most likely to be wrong.
type eventStream interface {
	Err() error
	Close() error
}

// OpenStream implements ports.StreamProvider.
//
// The nil-pool check is not decoration. This method is reached on the streaming path
// WITHOUT going through the handler's provider seam, so a process that never called
// gateway.Wire (or a test that stubs only the buffered call) would otherwise dereference a
// nil pool inside the response producer — a panic in a goroutine that is already writing to
// the client, which is the worst place to have one. An error here just falls back to the
// buffered call.
func (a *Adapter) OpenStream(ctx context.Context, in ports.InvokeInput) (ports.ProviderStream, error) {
	if a.Pool == nil {
		return nil, fmt.Errorf("bedrock: no client pool configured")
	}
	return openStream(ctx, a.Pool.ClientFor(ctx, a.Route), a.ModelID, in.Messages, in.Tools, a.CachePrefix)
}

func openStream(ctx context.Context, cli converseStreamAPI, modelID string, msgs []ports.Message, tools []ports.ToolDef, cachePrefix bool) (ports.ProviderStream, error) {
	system, conv := convertMessages(msgs)
	if cachePrefix && len(system) > 0 {
		system = append(system, &btypes.SystemContentBlockMemberCachePoint{
			Value: btypes.CachePointBlock{Type: btypes.CachePointTypeDefault},
		})
	}
	input := &bedrockruntime.ConverseStreamInput{ModelId: &modelID, Messages: conv}
	if len(system) > 0 {
		input.System = system
	}
	if tc := ConvertToolsToBedrockConfig(tools); tc != nil {
		input.ToolConfig = tc
	}

	// This call returns once the response headers are in, BEFORE the model has produced
	// tokens. That is what makes it usable as the "open and validate" phase: a bad model
	// id, a missing permission or a throttle surfaces here, while the caller can still
	// try another route.
	out, err := cli.ConverseStream(ctx, input)
	if err != nil {
		return nil, err
	}
	es := out.GetStream()
	if es == nil {
		return nil, fmt.Errorf("bedrock returned no event stream")
	}
	return &brStream{es: es, events: es.Events(), res: ports.Result{CacheCounters: ports.CacheCountersAbsent}}, nil
}

// brStream adapts the SDK's event channel to ports.ProviderStream.
type brStream struct {
	es     eventStream
	events <-chan btypes.ConverseStreamOutput

	res  ports.Result
	done bool

	// Tool calls arrive as a start event (id + name) followed by input JSON in
	// fragments, keyed by content block index. They are accumulated per index and
	// assembled when the stream ends, because a fragment on its own is not valid JSON.
	tools map[int32]*toolAccum
	order []int32
}

type toolAccum struct {
	id, name string
	args     []byte
}

// Recv drains events until it has text to return, the stream ends, or it breaks.
//
// Non-text events (message start/stop, block start/stop, metadata, tool input fragments)
// are consumed silently and update the accumulated Result — returning "" for them would
// make the caller spin.
func (s *brStream) Recv() (string, error) {
	if s.done {
		return "", io.EOF
	}
	for ev := range s.events {
		switch e := ev.(type) {
		case *btypes.ConverseStreamOutputMemberContentBlockDelta:
			switch d := e.Value.Delta.(type) {
			case *btypes.ContentBlockDeltaMemberText:
				if d.Value != "" {
					s.res.Text += d.Value
					return d.Value, nil
				}
			case *btypes.ContentBlockDeltaMemberToolUse:
				if acc := s.accFor(e.Value.ContentBlockIndex); acc != nil {
					acc.args = append(acc.args, aws.ToString(d.Value.Input)...)
				}
			}

		case *btypes.ConverseStreamOutputMemberContentBlockStart:
			if st, ok := e.Value.Start.(*btypes.ContentBlockStartMemberToolUse); ok {
				idx := aws.ToInt32(e.Value.ContentBlockIndex)
				if s.tools == nil {
					s.tools = map[int32]*toolAccum{}
				}
				if _, seen := s.tools[idx]; !seen {
					s.order = append(s.order, idx)
				}
				s.tools[idx] = &toolAccum{id: aws.ToString(st.Value.ToolUseId), name: aws.ToString(st.Value.Name)}
			}

		case *btypes.ConverseStreamOutputMemberMessageStop:
			s.res.StopReason = string(e.Value.StopReason)

		case *btypes.ConverseStreamOutputMemberMetadata:
			// The trailing usage event. Cache counters are read the same way as the
			// buffered path, including the reported/absent distinction: treating "the
			// provider did not say" as zero would hide real prompt-cache savings.
			if u := e.Value.Usage; u != nil {
				s.res.InputTokens = int(aws.ToInt32(u.InputTokens))
				s.res.OutputTokens = int(aws.ToInt32(u.OutputTokens))
				if u.CacheReadInputTokens != nil || u.CacheWriteInputTokens != nil {
					s.res.CacheReadInputTokens = int(aws.ToInt32(u.CacheReadInputTokens))
					s.res.CacheWriteInputTokens = int(aws.ToInt32(u.CacheWriteInputTokens))
					s.res.CacheCounters = ports.CacheCountersReported
				}
			}
		}
	}

	// Channel closed: either the model finished or the stream broke. Err() is the only
	// place the second case shows up — the channel closes identically either way, so
	// skipping this check would report a truncated answer as a complete one.
	s.done = true
	s.finish()
	if err := s.es.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

func (s *brStream) accFor(idx *int32) *toolAccum {
	if s.tools == nil {
		return nil
	}
	return s.tools[aws.ToInt32(idx)]
}

// finish assembles the accumulated tool calls, in the order the model opened them.
func (s *brStream) finish() {
	for _, idx := range s.order {
		acc := s.tools[idx]
		if acc == nil || acc.id == "" {
			continue
		}
		args := string(acc.args)
		// An empty or unparseable accumulation becomes {} rather than being passed
		// through: a client running tool calls would otherwise get invalid JSON, and
		// the buffered path already normalizes null to {}.
		if args == "" || !json.Valid(acc.args) {
			args = "{}"
		}
		s.res.ToolCalls = append(s.res.ToolCalls, ports.ToolCall{ID: acc.id, Name: acc.name, Arguments: args})
	}
}

func (s *brStream) Result() ports.Result { return s.res }

func (s *brStream) Close() error { return s.es.Close() }
