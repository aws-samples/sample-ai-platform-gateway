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
// text delta the frame carries, or "" for a frame that carries none — every dialect has
// bookkeeping events (message start, ping, usage) that produce no visible output.
//
// The Result is passed by pointer rather than returned so that a frame can update one
// field without having to restate the rest. Usage arrives spread across several frames
// in both dialects: Anthropic reports input tokens at the start and output tokens at the
// end, and Google repeats a cumulative block on every chunk.
type OnData func(data string, res *ports.Result) (text string)

// Stream implements ports.ProviderStream over an SSE body.
type Stream struct {
	resp   *http.Response
	sc     *bufio.Scanner
	onData OnData
	res    ports.Result
	done   bool
}

var _ ports.ProviderStream = (*Stream)(nil) // compile-time assertion

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

// Recv reads frames until one yields text, the stream ends, or it breaks.
//
// Frames that carry no text are consumed silently and may still update the Result —
// returning "" for them would make the caller spin on bookkeeping events.
func (s *Stream) Recv() (string, error) {
	if s.done {
		return "", io.EOF
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
		if text := s.onData(data, &s.res); text != "" {
			s.res.Text += text
			return text, nil
		}
	}
	s.done = true
	// A scanner error is a stream that broke mid-answer, which is NOT the same as a
	// stream that finished. Reporting EOF for both would record a truncated answer as a
	// complete one, and the caller bills what it received either way.
	if err := s.sc.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
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
