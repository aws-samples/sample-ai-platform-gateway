// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrock

// These tests drive the ConverseStream event translation without the network, which is the
// whole reason brStream takes an eventStream interface and a channel instead of the SDK's
// concrete stream. What they pin is the part that has no second chance at runtime: usage
// arrives in a TRAILING event, tool input arrives in FRAGMENTS that are not individually
// valid JSON, and a broken stream closes the channel exactly like a clean one.

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/aiplat/core/internal/ports"
)

// fakeES stands in for ConverseStreamEventStream, which reads from a live HTTP body.
type fakeES struct {
	err    error
	closed bool
}

func (f *fakeES) Err() error   { return f.err }
func (f *fakeES) Close() error { f.closed = true; return nil }

// newTestStream builds a brStream over a closed channel pre-loaded with events, so Recv
// sees exactly this sequence and then the end of stream.
func newTestStream(es *fakeES, events ...btypes.ConverseStreamOutput) *brStream {
	ch := make(chan btypes.ConverseStreamOutput, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return &brStream{es: es, events: ch, res: ports.Result{CacheCounters: ports.CacheCountersAbsent}}
}

func textDelta(s string) btypes.ConverseStreamOutput {
	return &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &btypes.ContentBlockDeltaMemberText{Value: s},
	}}
}

func drain(t *testing.T, s *brStream) ([]string, error) {
	t.Helper()
	var got []string
	for i := 0; i < 100; i++ {
		txt, err := s.Recv()
		if txt != "" {
			got = append(got, txt)
		}
		if err != nil {
			return got, err
		}
	}
	t.Fatal("Recv did not terminate after 100 iterations")
	return nil, nil
}

// TestStreamTextAndUsage is the ordinary case: deltas come out one by one, and the usage
// from the trailing metadata event is what Result reports.
func TestStreamTextAndUsage(t *testing.T) {
	s := newTestStream(&fakeES{},
		&btypes.ConverseStreamOutputMemberMessageStart{Value: btypes.MessageStartEvent{Role: btypes.ConversationRoleAssistant}},
		textDelta("Hello"),
		textDelta(", "),
		textDelta("world"),
		&btypes.ConverseStreamOutputMemberContentBlockStop{Value: btypes.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(0)}},
		&btypes.ConverseStreamOutputMemberMessageStop{Value: btypes.MessageStopEvent{StopReason: btypes.StopReason("end_turn")}},
		&btypes.ConverseStreamOutputMemberMetadata{Value: btypes.ConverseStreamMetadataEvent{
			Usage: &btypes.TokenUsage{InputTokens: aws.Int32(12), OutputTokens: aws.Int32(34)},
		}},
	)

	got, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final error = %v, expected io.EOF on a clean end", err)
	}
	if len(got) != 3 || got[0] != "Hello" || got[1] != ", " || got[2] != "world" {
		t.Errorf("deltas = %q, expected three separate deltas in order", got)
	}
	// One delta per event is the point: joining them here and returning once would be
	// indistinguishable from the buffered path.
	res := s.Result()
	if res.Text != "Hello, world" {
		t.Errorf("Text = %q, expected the concatenation", res.Text)
	}
	if res.InputTokens != 12 || res.OutputTokens != 34 {
		t.Errorf("tokens = %d/%d, expected 12/34 from the metadata event", res.InputTokens, res.OutputTokens)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, expected end_turn", res.StopReason)
	}
	// Nothing reported → absent, not zero. Treating this as "zero cached tokens" would
	// silently price a provider that simply does not report as if it never cached.
	if res.CacheCounters != ports.CacheCountersAbsent {
		t.Errorf("CacheCounters = %q, expected absent when the provider reported none", res.CacheCounters)
	}
	// Recv past the end must keep returning EOF rather than blocking on a closed channel.
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after the end = %v, expected io.EOF", err)
	}
}

// TestStreamCacheCountersReported pins the reported/absent distinction the cost model
// depends on: with prompt caching on, these counters are the difference between billing a
// cache read at the cache rate and not billing it at all.
func TestStreamCacheCountersReported(t *testing.T) {
	s := newTestStream(&fakeES{},
		textDelta("hi"),
		&btypes.ConverseStreamOutputMemberMetadata{Value: btypes.ConverseStreamMetadataEvent{
			Usage: &btypes.TokenUsage{
				InputTokens: aws.Int32(100), OutputTokens: aws.Int32(5),
				CacheReadInputTokens: aws.Int32(900),
			},
		}},
	)
	if _, err := drain(t, s); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	res := s.Result()
	if res.CacheCounters != ports.CacheCountersReported {
		t.Fatalf("CacheCounters = %q, expected reported", res.CacheCounters)
	}
	if res.CacheReadInputTokens != 900 {
		t.Errorf("CacheReadInputTokens = %d, expected 900", res.CacheReadInputTokens)
	}
	// A nil write counter alongside a non-nil read one must read as 0, not panic.
	if res.CacheWriteInputTokens != 0 {
		t.Errorf("CacheWriteInputTokens = %d, expected 0", res.CacheWriteInputTokens)
	}
}

// TestStreamToolUseFragments is the case that cannot be handled event by event: Bedrock
// sends tool input as a sequence of string fragments, and no fragment on its own parses.
func TestStreamToolUseFragments(t *testing.T) {
	toolStart := func(idx int32, id, name string) btypes.ConverseStreamOutput {
		return &btypes.ConverseStreamOutputMemberContentBlockStart{Value: btypes.ContentBlockStartEvent{
			ContentBlockIndex: aws.Int32(idx),
			Start:             &btypes.ContentBlockStartMemberToolUse{Value: btypes.ToolUseBlockStart{ToolUseId: aws.String(id), Name: aws.String(name)}},
		}}
	}
	toolFrag := func(idx int32, frag string) btypes.ConverseStreamOutput {
		return &btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
			ContentBlockIndex: aws.Int32(idx),
			Delta:             &btypes.ContentBlockDeltaMemberToolUse{Value: btypes.ToolUseBlockDelta{Input: aws.String(frag)}},
		}}
	}

	s := newTestStream(&fakeES{},
		toolStart(1, "call-1", "get_weather"),
		toolFrag(1, `{"ci`),
		toolFrag(1, `ty":"S`),
		toolFrag(1, `ao Paulo"}`),
		toolStart(2, "call-2", "get_time"),
		toolFrag(2, `{}`),
		&btypes.ConverseStreamOutputMemberMessageStop{Value: btypes.MessageStopEvent{StopReason: btypes.StopReason("tool_use")}},
	)

	got, err := drain(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tool fragments must not be emitted as text deltas, got %q", got)
	}

	res := s.Result()
	if len(res.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, expected 2", len(res.ToolCalls))
	}
	// Order follows the block indexes the model opened, not map iteration order — a map
	// would shuffle the calls between runs and break a client that pairs them positionally.
	if res.ToolCalls[0].ID != "call-1" || res.ToolCalls[1].ID != "call-2" {
		t.Errorf("tool calls out of order: %s, %s", res.ToolCalls[0].ID, res.ToolCalls[1].ID)
	}
	if res.ToolCalls[0].Name != "get_weather" {
		t.Errorf("Name = %q", res.ToolCalls[0].Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(res.ToolCalls[0].Arguments), &args); err != nil {
		t.Fatalf("assembled arguments are not valid JSON: %v (%s)", err, res.ToolCalls[0].Arguments)
	}
	if args["city"] != "Sao Paulo" {
		t.Errorf("arguments = %v, expected the fragments joined in order", args)
	}
}

// TestStreamTruncatedArgumentsNormalized: a stream cut mid-tool-input leaves invalid JSON.
// Passing it through would hand a client something it cannot parse, so it becomes {}.
func TestStreamTruncatedArgumentsNormalized(t *testing.T) {
	s := newTestStream(&fakeES{},
		&btypes.ConverseStreamOutputMemberContentBlockStart{Value: btypes.ContentBlockStartEvent{
			ContentBlockIndex: aws.Int32(0),
			Start:             &btypes.ContentBlockStartMemberToolUse{Value: btypes.ToolUseBlockStart{ToolUseId: aws.String("t"), Name: aws.String("f")}},
		}},
		&btypes.ConverseStreamOutputMemberContentBlockDelta{Value: btypes.ContentBlockDeltaEvent{
			ContentBlockIndex: aws.Int32(0),
			Delta:             &btypes.ContentBlockDeltaMemberToolUse{Value: btypes.ToolUseBlockDelta{Input: aws.String(`{"a":`)}},
		}},
	)
	drain(t, s)
	res := s.Result()
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Arguments != "{}" {
		t.Errorf("Arguments = %q, expected {} for a truncated fragment", res.ToolCalls[0].Arguments)
	}
}

// TestStreamBrokenSurfacesError is the failure mode that is invisible without checking
// Err(): the SDK closes the event channel the same way whether the model finished or the
// connection broke. Reporting EOF here would record a truncated answer as complete.
func TestStreamBrokenSurfacesError(t *testing.T) {
	boom := errors.New("connection reset by peer")
	es := &fakeES{err: boom}
	s := newTestStream(es, textDelta("partial answer"))

	got, err := drain(t, s)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, expected the event stream error rather than io.EOF", err)
	}
	// What was received is kept: those tokens were served and have to be billed.
	if len(got) != 1 || s.Result().Text != "partial answer" {
		t.Errorf("partial content lost: deltas=%q text=%q", got, s.Result().Text)
	}
	if err := s.Close(); err != nil || !es.closed {
		t.Errorf("Close did not reach the event stream (err=%v closed=%v)", err, es.closed)
	}
}
