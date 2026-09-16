// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aiplat/core/internal/ports"
)

// scriptStream replays a fixed sequence of chunks, optionally pausing before one of them so
// the silence the keepalive exists for can be reproduced without waiting on a real model.
type scriptStream struct {
	chunks  []ports.Chunk
	pause   time.Duration
	pauseAt int
	i       int
	res     ports.Result
	closed  bool
}

func (s *scriptStream) RecvChunk() (ports.Chunk, error) {
	if s.i >= len(s.chunks) {
		return ports.Chunk{}, io.EOF
	}
	if s.i == s.pauseAt && s.pause > 0 {
		time.Sleep(s.pause)
	}
	c := s.chunks[s.i]
	s.i++
	s.res.Text += c.Text
	s.res.Reasoning += c.Reasoning
	return c, nil
}
func (s *scriptStream) Recv() (string, error) {
	for {
		c, err := s.RecvChunk()
		if c.Text != "" || err != nil {
			return c.Text, err
		}
	}
}
func (s *scriptStream) Result() ports.Result { return s.res }
func (s *scriptStream) Close() error         { s.closed = true; return nil }

// textOnlyStream implements ONLY ports.ProviderStream, to prove the pump still works for an
// adapter that knows nothing about reasoning.
type textOnlyStream struct {
	parts []string
	i     int
	res   ports.Result
}

func (s *textOnlyStream) Recv() (string, error) {
	if s.i >= len(s.parts) {
		return "", io.EOF
	}
	p := s.parts[s.i]
	s.i++
	s.res.Text += p
	return p, nil
}
func (s *textOnlyStream) Result() ports.Result { return s.res }
func (s *textOnlyStream) Close() error         { return nil }

// frames splits an SSE body into its data payloads, dropping comment frames.
func frames(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "[DONE]" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("unparseable frame %q: %v", payload, err)
		}
		out = append(out, m)
	}
	return out
}

// deltaField reads choices[0].delta[key] from a frame, "" when absent.
func deltaField(m map[string]interface{}, key string) string {
	ch, _ := m["choices"].([]interface{})
	if len(ch) == 0 {
		return ""
	}
	c0, _ := ch[0].(map[string]interface{})
	d, _ := c0["delta"].(map[string]interface{})
	s, _ := d[key].(string)
	return s
}

// ── reasoning must never land in content ──────────────────────────────────────
//
// Every OpenAI SDK concatenates delta.content. If reasoning were emitted there, the model's
// chain of thought would be printed inside the answer — so this is the assertion that matters
// more than the presence of the field.
func TestPumpProviderStream_ReasoningGoesToItsOwnField(t *testing.T) {
	st := &scriptStream{chunks: []ports.Chunk{
		{Reasoning: "thinking about it"},
		{Signature: "sig-1"},
		{Text: "Answer."},
	}}
	var buf bytes.Buffer
	res := pumpProviderStream(context.Background(), &buf, "id-1", "m1", st, 0)

	var content, reasoning strings.Builder
	for _, f := range frames(t, buf.String()) {
		content.WriteString(deltaField(f, "content"))
		reasoning.WriteString(deltaField(f, "reasoning_content"))
	}
	if content.String() != "Answer." {
		t.Errorf("content = %q; reasoning must not leak into it", content.String())
	}
	if reasoning.String() != "thinking about it" {
		t.Errorf("reasoning_content = %q", reasoning.String())
	}
	// The signature is opaque metadata, not content for the client to render.
	if strings.Contains(buf.String(), "sig-1") {
		t.Error("the reasoning signature must not be forwarded to the client as content")
	}
	if res.Text != "Answer." {
		t.Errorf("accounted text = %q", res.Text)
	}
	if !st.closed {
		t.Error("the pump must close the provider stream")
	}
}

// An adapter that does not implement ports.ReasoningStream must keep working unchanged.
func TestPumpProviderStream_TextOnlyAdapterStillWorks(t *testing.T) {
	var buf bytes.Buffer
	pumpProviderStream(context.Background(), &buf, "id-2", "m1", &textOnlyStream{parts: []string{"a", "b"}}, 0)
	var content strings.Builder
	for _, f := range frames(t, buf.String()) {
		content.WriteString(deltaField(f, "content"))
	}
	if content.String() != "ab" {
		t.Errorf("content = %q", content.String())
	}
	if strings.Contains(buf.String(), "reasoning_content") {
		t.Error("a text-only stream must not produce reasoning frames")
	}
}

// ── the keepalive ─────────────────────────────────────────────────────────────
//
// Idle timeouts count bytes, not progress. This reproduces the dangerous window — silence
// before the first token — and asserts a comment frame goes out during it.
func TestPumpProviderStream_KeepalivePingsDuringSilence(t *testing.T) {
	st := &scriptStream{
		chunks:  []ports.Chunk{{Text: "finally"}},
		pause:   120 * time.Millisecond,
		pauseAt: 0,
	}
	var buf bytes.Buffer
	pumpProviderStream(context.Background(), &buf, "id-3", "m1", st, 20*time.Millisecond)

	if !strings.Contains(buf.String(), ": ping") {
		t.Fatalf("expected a keepalive comment frame during the silence, got:\n%s", buf.String())
	}
	// A ping is a COMMENT: it must not be a chunk, so it cannot be mistaken for content or
	// disturb a client that only parses data frames.
	for _, f := range frames(t, buf.String()) {
		if deltaField(f, "content") == ": ping" {
			t.Error("the ping leaked into a data frame")
		}
	}
	// And the answer still arrives after the pings.
	var content strings.Builder
	for _, f := range frames(t, buf.String()) {
		content.WriteString(deltaField(f, "content"))
	}
	if content.String() != "finally" {
		t.Errorf("content = %q", content.String())
	}
}

func TestPumpProviderStream_NoPingWhenDisabledOrBusy(t *testing.T) {
	// Disabled: zero must mean off, not "ping constantly".
	var off bytes.Buffer
	pumpProviderStream(context.Background(), &off, "id-4", "m1",
		&scriptStream{chunks: []ports.Chunk{{Text: "x"}}, pause: 60 * time.Millisecond, pauseAt: 0}, 0)
	if strings.Contains(off.String(), ": ping") {
		t.Error("keepalive 0 must disable the ping entirely")
	}

	// Busy: real traffic flowing means a ping would be noise.
	var busy bytes.Buffer
	chunks := make([]ports.Chunk, 40)
	for i := range chunks {
		chunks[i] = ports.Chunk{Text: "t"}
	}
	pumpProviderStream(context.Background(), &busy, "id-5", "m1",
		&scriptStream{chunks: chunks}, 50*time.Millisecond)
	if strings.Contains(busy.String(), ": ping") {
		t.Error("no ping should be emitted while frames are flowing")
	}
}

// ── the deadline ──────────────────────────────────────────────────────────────
//
// The point is not that generation stops — it is that the client receives a WELL-FORMED end.
// A truncated stream leaves an OpenAI SDK waiting; stop → final → [DONE] does not.
func TestPumpProviderStream_DeadlineEndsTheStreamCleanly(t *testing.T) {
	st := &scriptStream{
		chunks:  []ports.Chunk{{Text: "first "}, {Text: "never arrives"}},
		pause:   2 * time.Second,
		pauseAt: 1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := pumpProviderStream(ctx, &bytes.Buffer{}, "id-6", "m1", st, 0)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the deadline did not stop the pump: took %s", elapsed)
	}
	// What was served is real and was billed, so it must be accounted, not discarded.
	if res.Text != "first " {
		t.Errorf("accounted text = %q, want what actually reached the client", res.Text)
	}
}

func TestPumpProviderStream_DeadlineStillEmitsStop(t *testing.T) {
	st := &scriptStream{chunks: []ports.Chunk{{Text: "hi"}, {Text: "late"}}, pause: 2 * time.Second, pauseAt: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	pumpProviderStream(ctx, &buf, "id-7", "m1", st, 0)

	sawStop := false
	for _, f := range frames(t, buf.String()) {
		ch, _ := f["choices"].([]interface{})
		if len(ch) == 0 {
			continue
		}
		c0, _ := ch[0].(map[string]interface{})
		if fr, _ := c0["finish_reason"].(string); fr == "stop" {
			sawStop = true
		}
	}
	if !sawStop {
		t.Error("a deadline must still close the stream with a stop frame, not truncate it")
	}
}

// ── the clamp ─────────────────────────────────────────────────────────────────
//
// A scope may only NARROW the runtime's ceiling. The ceiling is read from the runtime's own
// deadline so it cannot drift out of sync with the deployed Lambda timeout.
func TestStreamDeadline_ScopeCanOnlyNarrow(t *testing.T) {
	// Runtime has 10s left; the margin reserves time for stop/final/[DONE]/accounting.
	runtime, cancel := context.WithTimeout(context.Background(), 10*time.Second+streamDeadlineMargin)
	defer cancel()

	// A shorter scope value wins.
	short, c1 := streamDeadline(runtime, 2000)
	defer c1()
	dl, ok := short.Deadline()
	if !ok {
		t.Fatal("expected a deadline")
	}
	if d := time.Until(dl); d > 3*time.Second {
		t.Errorf("scope value of 2s was not applied: %s", d)
	}

	// A longer scope value is IGNORED — it cannot raise the runtime's ceiling.
	long, c2 := streamDeadline(runtime, 600000)
	defer c2()
	dl2, ok := long.Deadline()
	if !ok {
		t.Fatal("expected a deadline")
	}
	if d := time.Until(dl2); d > 11*time.Second {
		t.Errorf("a scope asking for 600s must be clamped to the runtime ceiling, got %s", d)
	}

	// Zero means "no scope deadline": still clamped by the runtime, never unbounded.
	zero, c3 := streamDeadline(runtime, 0)
	defer c3()
	if _, ok := zero.Deadline(); !ok {
		t.Error("with a runtime deadline present the stream must still be bounded")
	}
}

func TestStreamDeadline_NoRuntimeDeadlineHonoursScope(t *testing.T) {
	ctx, cancel := streamDeadline(context.Background(), 1500)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a scope value must apply even with no runtime deadline")
	}
	if d := time.Until(dl); d > 2*time.Second {
		t.Errorf("got %s", d)
	}

	// Nothing to bound it with: the caller still gets a cancellable context.
	plain, cancel2 := streamDeadline(context.Background(), 0)
	defer cancel2()
	if _, ok := plain.Deadline(); ok {
		t.Error("with neither a runtime deadline nor a scope value there is nothing to clamp to")
	}
}

// The default is the load-bearing part of the keepalive: the failure it prevents does not
// look like a timeout to whoever hits it, it looks like the gateway dropping a good request.
// A feature that had to be discovered and switched on would leave every default deployment
// with the bug, so "never configured" must resolve to ON — and that forces "off" to be
// expressed some other way, because an int with omitempty cannot distinguish 0 from unset.
func TestEffectiveKeepalive(t *testing.T) {
	if got := effectiveKeepalive(0); got != defaultSSEKeepalive {
		t.Errorf("unset gave %v, want the default %v — a deployment that never configured this must still be protected", got, defaultSSEKeepalive)
	}
	if got := effectiveKeepalive(-1); got != 0 {
		t.Errorf("negative gave %v, want 0 (explicitly disabled)", got)
	}
	if got := effectiveKeepalive(10); got != 10*time.Second {
		t.Errorf("10 gave %v, want 10s", got)
	}
	// Under every idle timer this has to survive: 60s is the common proxy default and 30s
	// is an edge-optimized API Gateway, so the default must leave room for a lost frame.
	if defaultSSEKeepalive >= 30*time.Second {
		t.Errorf("default %v is not below the 30s edge-optimized idle timeout", defaultSSEKeepalive)
	}
}

// slowStream keeps producing AFTER the pump has given up, which is what a real provider does:
// cancelling the pump's deadline does not cancel the provider's HTTP stream, since that was
// opened with the request context. So the adapter goes on accumulating into its Result while
// the pump is finishing up.
type slowStream struct {
	delay time.Duration
	n     int
	res   ports.Result
}

func (s *slowStream) Recv() (string, error) {
	time.Sleep(s.delay)
	s.n++
	// The write the race is about: the adapter mutating its accumulated Result.
	s.res.Text += "x"
	s.res.OutputTokens++
	return "x", nil
}
func (s *slowStream) Result() ports.Result { return s.res }
func (s *slowStream) Close() error         { return nil }

// Run with -race, this fails on a pump that reads st.Result() at the end: the reader goroutine
// is still inside the adapter when the deadline fires, so the final read and the adapter's
// write overlap. The failure mode without the detector is worse than a crash — a torn read of
// the token counters is a wrong number in the ledger, and nothing reports it.
func TestPumpProviderStream_NoRaceOnResultWhenDeadlineFiresMidRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	var buf bytes.Buffer
	st := &slowStream{delay: 15 * time.Millisecond}
	res := pumpProviderStream(ctx, &buf, "id-1", "m", st, 0)

	// Give the abandoned reader time to keep mutating: if the pump had handed back the
	// adapter's live Result, this is the window in which the value would also change under
	// the caller while it is being written to the ledger.
	time.Sleep(60 * time.Millisecond)

	// What is asserted is consistency, not an exact count: the snapshot must describe a real
	// moment of the stream, so the text and the counter have to agree with each other.
	if len(res.Text) != res.OutputTokens {
		t.Errorf("snapshot is internally inconsistent: %d chars vs %d tokens", len(res.Text), res.OutputTokens)
	}
	// The client must still see a well-formed end rather than a truncated stream.
	if !strings.Contains(buf.String(), `"finish_reason":"stop"`) {
		t.Error("an abandoned stream must still be closed with a stop frame")
	}
}

// pseudoStream is the path a provider WITHOUT a native streaming API takes — every
// OpenAI-compatible route, since that adapter has no OpenStream. It emitted only the answer, so
// reasoning_content reached a streaming client on every provider except through this fallback:
// the reasoning was parsed, billed and recorded in the ledger, and never shown.
func TestPseudoStream_EmitsReasoningBeforeTheAnswer(t *testing.T) {
	var buf bytes.Buffer
	pseudoStream(&buf, "id-1", "m", "thinking about it", "The answer.")

	var reasoning, content strings.Builder
	firstReasoningAt, firstContentAt := -1, -1
	for i, f := range frames(t, buf.String()) {
		ch, _ := f["choices"].([]interface{})
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]interface{})["delta"].(map[string]interface{})
		if s, ok := d["reasoning_content"].(string); ok && s != "" {
			reasoning.WriteString(s)
			if firstReasoningAt < 0 {
				firstReasoningAt = i
			}
		}
		if s, ok := d["content"].(string); ok && s != "" {
			content.WriteString(s)
			if firstContentAt < 0 {
				firstContentAt = i
			}
		}
	}
	if reasoning.String() != "thinking about it" {
		t.Errorf("reasoning_content = %q, want the full reasoning", reasoning.String())
	}
	if content.String() != "The answer." {
		t.Errorf("content = %q; reasoning must not leak into it", content.String())
	}
	// Ordering matters: it is what a native reasoning stream produces, so a client that
	// renders progressively behaves the same on both paths.
	if firstReasoningAt < 0 || firstContentAt < 0 || firstReasoningAt > firstContentAt {
		t.Errorf("reasoning must be emitted BEFORE the answer (reasoning@%d, content@%d)", firstReasoningAt, firstContentAt)
	}
}

// No reasoning must produce exactly what it produced before this parameter existed.
func TestPseudoStream_WithoutReasoningIsUnchanged(t *testing.T) {
	var buf bytes.Buffer
	pseudoStream(&buf, "id-1", "m", "", "hello world")
	if strings.Contains(buf.String(), "reasoning_content") {
		t.Error("no reasoning must mean no reasoning frames")
	}
	var content strings.Builder
	for _, f := range frames(t, buf.String()) {
		ch, _ := f["choices"].([]interface{})
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]interface{})["delta"].(map[string]interface{})
		if s, ok := d["content"].(string); ok {
			content.WriteString(s)
		}
	}
	if content.String() != "hello world" {
		t.Errorf("content = %q", content.String())
	}
}

// A cached answer produced WITH thinking must replay as a thinking answer. Replaying only
// `content` would make the same question, answered from cache, look like a non-reasoning
// response to a streaming client.
func TestCachedMessageParts_ExtractsBothHalves(t *testing.T) {
	cached := map[string]interface{}{
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{
				"content":           "42",
				"reasoning_content": "let me count",
			},
		}},
	}
	r, txt := cachedMessageParts(cached)
	if r != "let me count" || txt != "42" {
		t.Errorf("got reasoning=%q text=%q, want both halves", r, txt)
	}
	// A malformed or empty body must not panic — a cache hit is the hot path.
	for _, bad := range []map[string]interface{}{
		{}, {"choices": []interface{}{}}, {"choices": []interface{}{"not a map"}},
		{"choices": []interface{}{map[string]interface{}{}}},
	} {
		if r, txt := cachedMessageParts(bad); r != "" || txt != "" {
			t.Errorf("malformed body %v gave %q/%q", bad, r, txt)
		}
	}
}
