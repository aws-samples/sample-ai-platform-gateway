// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrockinvoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/aiplat/core/internal/ports"
)

// invokeStreamPath is the native InvokeModelWithResponseStream path. Measured 2026-10-26:
// it returns the AWS binary event-stream (content-type application/vnd.amazon.eventstream)
// carrying Converse-shaped chunks (messageStart, contentBlockDelta, contentBlockStop,
// messageStop, metadata), the same shape the Bedrock ConverseStream path emits.
func invokeStreamPath(modelID string) string {
	return "/inference/model/" + modelID + "/invoke-with-response-stream"
}

var _ ports.StreamProvider = (*Adapter)(nil) // compile-time assertion

// OpenStream opens the native streaming path. It validates up front (auth, model id,
// status) so a failure can still fall back to another route before the client's HTTP
// status is committed, as the StreamProvider contract requires.
func (a *Adapter) OpenStream(ctx context.Context, in ports.InvokeInput) (ports.ProviderStream, error) {
	payload, err := json.Marshal(buildBody(in, a.CachePrefix))
	if err != nil {
		return nil, fmt.Errorf("bedrock_invoke: marshal request: %w", err)
	}
	endpoint := a.BaseURL + invokeStreamPath(a.ModelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("bedrock_invoke: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "application/vnd.amazon.eventstream")
	if in.RequestID != "" {
		req.Header.Set(correlationHeader, in.RequestID)
	}
	if err := a.Signer.SignRequest(ctx, req, payload, SigningService, a.Region); err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock_invoke: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, fmt.Errorf("bedrock_invoke: HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return newStream(resp.Body), nil
}

// stream drains a native InvokeModel event-stream one text delta at a time.
type stream struct {
	body io.ReadCloser
	dec  *eventstream.Decoder
	res  ports.Result
}

func newStream(body io.ReadCloser) *stream {
	return &stream{
		body: body,
		dec:  eventstream.NewDecoder(),
		res:  ports.Result{CacheCounters: ports.CacheCountersAbsent},
	}
}

// streamChunk is the inner Converse chunk, carried base64 inside each event-stream
// message's {"bytes": ...} payload. Only the fields this adapter needs are declared.
type streamChunk struct {
	ContentBlockDelta *struct {
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"contentBlockDelta"`
	MessageStop *struct {
		StopReason string `json:"stopReason"`
	} `json:"messageStop"`
	Metadata *struct {
		Usage *struct {
			InputTokens               *int `json:"inputTokens"`
			OutputTokens              *int `json:"outputTokens"`
			CacheReadInputTokenCount  *int `json:"cacheReadInputTokenCount"`
			CacheWriteInputTokenCount *int `json:"cacheWriteInputTokenCount"`
		} `json:"usage"`
	} `json:"metadata"`
}

// Recv returns the next answer text delta. io.EOF means the model finished normally. Usage
// and stop reason are captured as their chunks pass by and are readable from Result() once
// Recv has returned an error (usage arrives in a trailing metadata chunk).
func (s *stream) Recv() (string, error) {
	for {
		msg, err := s.dec.Decode(s.body, nil)
		if err != nil {
			if err == io.EOF {
				return "", io.EOF
			}
			return "", fmt.Errorf("bedrock_invoke: stream decode: %w", err)
		}
		// An exception message carries a provider error mid-stream.
		if mt := msg.Headers.Get(":message-type"); mt != nil && mt.String() == "exception" {
			return "", fmt.Errorf("bedrock_invoke: stream exception: %s", truncate(string(msg.Payload), 300))
		}
		// The event payload is {"bytes": <base64 of the inner JSON chunk>, ...}. encoding/json
		// decodes a base64 string into []byte, so this unwraps and decodes in one step.
		var wrap struct {
			Bytes []byte `json:"bytes"`
		}
		if json.Unmarshal(msg.Payload, &wrap) != nil || len(wrap.Bytes) == 0 {
			continue
		}
		var ch streamChunk
		if json.Unmarshal(wrap.Bytes, &ch) != nil {
			continue
		}
		if ch.MessageStop != nil && ch.MessageStop.StopReason != "" {
			s.res.StopReason = ch.MessageStop.StopReason
		}
		if ch.Metadata != nil && ch.Metadata.Usage != nil {
			u := ch.Metadata.Usage
			s.res.InputTokens = derefOr(u.InputTokens, s.res.InputTokens)
			s.res.OutputTokens = derefOr(u.OutputTokens, s.res.OutputTokens)
			if u.CacheReadInputTokenCount != nil || u.CacheWriteInputTokenCount != nil {
				s.res.CacheReadInputTokens = deref(u.CacheReadInputTokenCount)
				s.res.CacheWriteInputTokens = deref(u.CacheWriteInputTokenCount)
				s.res.CacheCounters = ports.CacheCountersReported
			}
		}
		if ch.ContentBlockDelta != nil && ch.ContentBlockDelta.Delta.Text != "" {
			s.res.Text += ch.ContentBlockDelta.Delta.Text
			return ch.ContentBlockDelta.Delta.Text, nil
		}
		// messageStart, contentBlockStart/Stop, and empty deltas carry no answer text: read
		// the next message.
	}
}

// Result is complete only after Recv has returned an error (usage is a trailing chunk).
func (s *stream) Result() ports.Result { return s.res }

func (s *stream) Close() error {
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

func derefOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
}
