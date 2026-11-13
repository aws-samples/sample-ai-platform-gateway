// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// AUD-BND-001: the cold-tier archive must serialise DynamoDB numbers as JSON
// numbers, not quoted strings. A regression to av.Number() would re-break the
// archive shape for Athena/Glue. This test fails behaviourally on the string
// encoding (it asserts the marshalled JSON type), not merely at compile time.
package main

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestArchiveNumbersAreJSONNumbers(t *testing.T) {
	rec := map[string]interface{}{
		"change_count": attrToInterface(events.NewNumberAttribute("3")),
		"truncated":    attrToInterface(events.NewNumberAttribute("0")),
		"action":       attrToInterface(events.NewStringAttribute("member_update")),
	}
	if _, ok := rec["change_count"].(float64); !ok {
		t.Fatalf("change_count must decode to a Go number, got %T", rec["change_count"])
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Re-decode WITHOUT forcing types: a JSON number becomes float64, a JSON
	// string becomes string. This is exactly what Athena/Glue see.
	var back map[string]interface{}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := back["change_count"].(float64); !ok {
		t.Fatalf("AUD-BND-001: change_count serialised as %T (want JSON number); archive shape regressed to strings", back["change_count"])
	}
	if _, ok := back["truncated"].(float64); !ok {
		t.Fatalf("AUD-BND-001: truncated serialised as %T (want JSON number)", back["truncated"])
	}
	// A string field must stay a string (sanity: we did not numberify everything).
	if _, ok := back["action"].(string); !ok {
		t.Fatalf("action must stay a JSON string, got %T", back["action"])
	}
}
