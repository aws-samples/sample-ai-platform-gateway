// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ssestream turns a provider's Server-Sent Events response into a
// ports.ProviderStream.
//
// It exists because Anthropic and Google both stream over SSE but speak different
// dialects: the transport mechanics (read a line, keep only `data:`, stop at the end,
// close the body exactly once, accumulate usage that arrives in a trailing event) are
// identical, and only the shape of each frame differs. Written twice, the two copies
// drift — and the part that drifts silently is the token accounting, because a stream
// that reports no usage still produces a perfectly readable answer.
//
// The per-provider part is one function: given a frame's data, append whatever text it
// carries and update the Result. Everything else lives here.
package ssestream

import (
	"bufio"
	"io"
	"net/http"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// OnData interprets one SSE frame in a provider's dialect.
//
// It receives the raw `data:` payload and a pointer to the Result being accumulated,
// which it may update in place (tokens, cache counters, stop reason). It returns the
// CHUNK the frame carries — answer text, reasoning text, or nothing at all, since every
// dialect has bookkeeping events (message start, ping, usage) that produce no output.
//
// It returns a ports.Chunk rather than a string because these providers stream chain of
// thought as its own event type. With a string return there was nowhere to put it: a
// dialect could either drop the reasoning or blend it into the answer, and the second is
// worse than the first — every OpenAI SDK concatenates delta.content, so the model's
// private thinking would print inside the answer an end user reads. Widening the seam is
// what lets Anthropic and Gemini expose reasoning through the same path Bedrock already
// does.
//
// The Result is passed by pointer rather than returned so that a frame can update one
// field without having to restate the rest. Usage arrives spread across several frames
// in both dialects: Anthropic reports input tokens at the start and output tokens at the
// end, and Google repeats a cumulative block on every chunk.
type OnData func(data string, res *ports.Result) ports.Chunk

// Stream implements ports.ProviderStream over an SSE body.
type Stream struct {
	resp   *http.Response
	sc     *bufio.Scanner
	onData OnData
	res    ports.Result
	done   bool
}

var (
	_ ports.ProviderStream  = (*Stream)(nil) // compile-time assertion
	_ ports.ReasoningStream = (*Stream)(nil) // dialects here can carry chain of thought
)

// New wraps an already-open, already-validated SSE response.
//
// The caller owns validating the status BEFORE calling this: a 4xx body is an error
// message, not a stream, and turning it into a Stream would deliver the provider's error
// text to the client as if it were the model's answer.
func New(resp *http.Response, onData OnData) *Stream {
	sc := bufio.NewScanner(resp.Body)
	// A single SSE line can be large (a whole JSON chunk, and Google repeats the usage
	// block on every one). The default 64 KB token limit would make the scanner stop mid
	// stream and look exactly like a provider that hung up early.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &Stream{resp: resp, sc: sc, onData: onData, res: ports.Result{CacheCounters: ports.CacheCountersAbsent}}
}

// Recv reads frames until one yields ANSWER text, the stream ends, or it breaks.
//
// Reasoning is skipped here rather than returned, for the same reason the Bedrock adapter
// skips it: a caller reading through Recv expects answer content and would print the chain
// of thought into the answer. Callers that want it use RecvChunk. Either way the reasoning
// is accumulated into the Result, so nothing is lost by reading the narrow method.
func (s *Stream) Recv() (string, error) {
	for {
		c, err := s.RecvChunk()
		if c.Text != "" || err != nil {
			return c.Text, err
		}
	}
}

// RecvChunk implements ports.ReasoningStream: it reads frames until one yields something
// for the caller — answer text, reasoning text, or a signature — or the stream ends.
//
// Frames that carry nothing are consumed silently and may still update the Result;
// returning an empty chunk for them would make the caller spin on bookkeeping events.
func (s *Stream) RecvChunk() (ports.Chunk, error) {
	if s.done {
		return ports.Chunk{}, io.EOF
	}
	for s.sc.Scan() {
		line := strings.TrimRight(s.sc.Text(), "\r")
		// Only data lines matter. `event:` names the type, but every dialect here also
		// carries the type inside the JSON, so the payload is self-describing and the
		// event line can be ignored. Blank lines are frame separators.
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		// "[DONE]" is the OpenAI terminator. Neither dialect here sends it, but a
		// gateway in front of them might, and treating it as JSON would just be a
		// parse error swallowed below — better to stop, which is what it means.
		if data == "" || data == "[DONE]" {
			continue
		}
		// Accumulation lives here, not in onData: every dialect would otherwise have to
		// remember to append, and forgetting produces an empty cached response with a
		// correct-looking stream.
		c := s.onData(data, &s.res)
		if c.Text != "" {
			s.res.Text += c.Text
		}
		if c.Reasoning != "" {
			s.res.Reasoning += c.Reasoning
			s.res.ReasoningChars += len(c.Reasoning)
		}
		if c.Signature != "" {
			s.res.ReasoningSignature = c.Signature
		}
		if c.Redacted {
			s.res.ReasoningRedacted = true
		}
		if c.Text != "" || c.Reasoning != "" || c.Signature != "" || c.Redacted {
			return c, nil
		}
	}
	s.done = true
	// A scanner error is a stream that broke mid-answer, which is NOT the same as a
	// stream that finished. Reporting EOF for both would record a truncated answer as a
	// complete one, and the caller bills what it received either way.
	if err := s.sc.Err(); err != nil {
		return ports.Chunk{}, err
	}
	return ports.Chunk{}, io.EOF
}

// Result returns the accumulated response. Complete only after Recv has returned an
// error, because both dialects report final token counts in a trailing frame.
func (s *Stream) Result() ports.Result { return s.res }

// Close releases the HTTP body. Safe to call more than once.
func (s *Stream) Close() error {
	if s.resp == nil || s.resp.Body == nil {
		return nil
	}
	err := s.resp.Body.Close()
	s.resp = nil
	return err
}
