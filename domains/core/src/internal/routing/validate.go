// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package routing

import (
	"encoding/json"
	"strings"

	"github.com/aiplat/core/internal/ports"
)

// Invalid response reasons (Req 13). All DETERMINISTIC.
const (
	InvalidMissingToolCalls = "missing_tool_calls"
	InvalidJSON             = "invalid_json"
	InvalidEmptyResponse    = "empty_response"
	InvalidTruncated        = "truncated_response"
)

// Validation is the structural verdict about a response.
type Validation struct {
	Valid  bool
	Reason string
}

// Validate classifies the response using ONLY structural checks.
//
// What this validator deliberately does NOT do: judge whether the response is good.
// A model-as-judge would cost money and latency on the hot path, and promising a
// quality we do not measure would be the same mistake as a ledger that ignores the
// cost of the retry. The console is required to state that the check is structural
// (Req 13.7).
func Validate(res ports.Result, req RequestShape, expectJSON bool) Validation {
	text := strings.TrimSpace(res.Text)
	hasTool := len(res.ToolCalls) > 0

	// Tools were requested and neither a tool call nor text came back: the model did
	// not understand the contract. This is the symptom of `arguments: {}`.
	if req.HasTools && !hasTool && text == "" {
		return Validation{false, InvalidMissingToolCalls}
	}
	if !hasTool && text == "" {
		return Validation{false, InvalidEmptyResponse}
	}
	// Truncated by the token limit: an incomplete response is invalid, not "short".
	switch strings.ToLower(res.StopReason) {
	case "max_tokens", "length":
		return Validation{false, InvalidTruncated}
	}
	if expectJSON && !hasTool && !json.Valid([]byte(text)) {
		return Validation{false, InvalidJSON}
	}
	// A tool call with an empty name is structurally broken.
	for _, tc := range res.ToolCalls {
		if strings.TrimSpace(tc.Name) == "" {
			return Validation{false, InvalidMissingToolCalls}
		}
	}
	return Validation{Valid: true}
}
