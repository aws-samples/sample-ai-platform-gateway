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
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

var (
	_ ports.StreamProvider  = (*Adapter)(nil)  // compile-time assertion
	_ ports.ReasoningStream = (*brStream)(nil) // this adapter does expose chain of thought
)

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
	return openStream(ctx, a.Pool.ClientFor(ctx, a.Route), a.ModelID, in, a.CachePrefix)
}

func openStream(ctx context.Context, cli converseStreamAPI, modelID string, in ports.InvokeInput, cachePrefix bool) (ports.ProviderStream, error) {
	system, conv := convertMessages(in.Messages)
	if cachePrefix && len(system) > 0 {
		system = append(system, &btypes.SystemContentBlockMemberCachePoint{
			Value: btypes.CachePointBlock{Type: btypes.CachePointTypeDefault},
		})
	}
	input := &bedrockruntime.ConverseStreamInput{ModelId: &modelID, Messages: conv}
	if len(system) > 0 {
		input.System = system
	}
	if tc := ConvertToolsToBedrockConfig(in.Tools); tc != nil {
		input.ToolConfig = tc
	}
	// Same helper as the buffered path, on purpose: ConverseInput and ConverseStreamInput
	// are distinct types with identical members, so two literals would drift and the
	// drifting half would be whichever one has less coverage.
	if cfg, extra := applyInference(in); cfg != nil || extra != nil {
		input.InferenceConfig = cfg
		input.AdditionalModelRequestFields = extra
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

	// warnedUnknown keeps the unrecognised-delta warning to one line per stream: the
	// point is to make a new SDK member visible, not to emit a log per token.
	warnedUnknown bool

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

// Recv drains events until it has ANSWER text to return, the stream ends, or it breaks.
//
// Reasoning is skipped here rather than returned, because a caller reading through Recv
// expects answer content and would print the chain of thought into the answer. Callers that
// want it use RecvChunk (ports.ReasoningStream); either way the reasoning is accumulated
// into Result.
func (s *brStream) Recv() (string, error) {
	for {
		c, err := s.RecvChunk()
		if c.Text != "" || err != nil {
			return c.Text, err
		}
	}
}

// RecvChunk implements ports.ReasoningStream: it drains events until it has SOMETHING for
// the caller — answer text, reasoning text, or a signature — or the stream ends.
//
// KNOWN BUG this fixes: the delta type switch handled Text and ToolUse and had no default,
// so `reasoningContent` was discarded silently. An extended-thinking model streams its chain
// of thought during the think phase, which means Bedrock WAS sending bytes the whole time and
// this adapter was throwing them away. The visible symptom was the opposite of the cause:
// "the model goes quiet for minutes" looked like a provider or transport problem, while the
// silence was manufactured here. The signature was dropped with it, which would have made a
// reasoning chain impossible to continue across turns later.
func (s *brStream) RecvChunk() (ports.Chunk, error) {
	if s.done {
		return ports.Chunk{}, io.EOF
	}
	for ev := range s.events {
		switch e := ev.(type) {
		case *btypes.ConverseStreamOutputMemberContentBlockDelta:
			switch d := e.Value.Delta.(type) {
			case *btypes.ContentBlockDeltaMemberText:
				if d.Value != "" {
					s.res.Text += d.Value
					return ports.Chunk{Text: d.Value}, nil
				}
			case *btypes.ContentBlockDeltaMemberReasoningContent:
				switch r := d.Value.(type) {
				case *btypes.ReasoningContentBlockDeltaMemberText:
					if r.Value != "" {
						s.res.Reasoning += r.Value
						s.res.ReasoningChars += len(r.Value)
						return ports.Chunk{Reasoning: r.Value}, nil
					}
				case *btypes.ReasoningContentBlockDeltaMemberSignature:
					// Not forwarded as content — it is opaque metadata. Kept because it
					// is the only thing that lets a later turn resume this reasoning.
					if r.Value != "" {
						s.res.ReasoningSignature = r.Value
						return ports.Chunk{Signature: r.Value}, nil
					}
				case *btypes.ReasoningContentBlockDeltaMemberRedactedContent:
					// Encrypted thinking: there IS reasoning, we just cannot read it.
					// Recorded so "did not think" stays distinguishable from "not shown".
					s.res.ReasoningRedacted = true
					return ports.Chunk{Redacted: true}, nil
				}
			case *btypes.ContentBlockDeltaMemberToolUse:
				if acc := s.accFor(e.Value.ContentBlockIndex); acc != nil {
					acc.args = append(acc.args, aws.ToString(d.Value.Input)...)
				}
			default:
				// The SDK defines six delta members and this switch acts on four. A
				// missing default is what made reasoningContent vanish without a trace,
				// so anything unrecognised is now logged ONCE per stream instead of
				// disappearing — the next member the SDK adds shows up as a log line
				// rather than as a user reporting silence.
				if !s.warnedUnknown {
					s.warnedUnknown = true
					fmt.Fprintf(os.Stderr,
						`{"lvl":"warn","evt":"bedrock_stream_unknown_delta","type":"%T"}`+"\n", d)
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
		return ports.Chunk{}, err
	}
	return ports.Chunk{}, io.EOF
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
