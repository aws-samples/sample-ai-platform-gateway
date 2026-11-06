// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test — the WRITER (decoder) side of the Usage_Record seam (OBS-BND-001 /
// XC-ARC-001). It decodes the shared canonical record and asserts buildItem persists
// every field the contract says it must, instead of dropping the ones the fixed struct
// did not name. The SAME fixture is asserted on the Core emitter side
// (core/internal/gateway) with no shared code — the fixture is what keeps the two in step.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const usageContractPath = "../../../../../testdata/contracts/usage-record/record-v1.json"

func TestUsageRecordContract_Writer(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(usageContractPath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var fx struct {
		Record            json.RawMessage `json:"record"`
		WriterMustPersist []string        `json:"writer_must_persist"`
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	var u usage
	if err := json.Unmarshal(fx.Record, &u); err != nil {
		t.Fatalf("the writer cannot decode the canonical record: %v", err)
	}
	item := buildItem(u)
	for _, k := range fx.WriterMustPersist {
		if _, ok := item[k]; !ok {
			t.Errorf("buildItem DROPPED contract field %q (OBS-BND-001)", k)
		}
	}
	// Spot-check that values round-tripped with the right type.
	if sv(item["schema"]) != "usage-record/1" {
		t.Errorf("schema = %q, want usage-record/1", sv(item["schema"]))
	}
	if sv(item["mode"]) != "sync" {
		t.Errorf("mode = %q, want sync", sv(item["mode"]))
	}
	if nv(item["reasoning_chars"]) != "529" {
		t.Errorf("reasoning_chars = %q, want 529", nv(item["reasoning_chars"]))
	}
	if sv(item["reasoning_effort"]) != "low" {
		t.Errorf("reasoning_effort = %q, want low", sv(item["reasoning_effort"]))
	}
	if nv(item["thinking_budget_tokens"]) != "1024" {
		t.Errorf("thinking_budget_tokens = %q, want 1024", nv(item["thinking_budget_tokens"]))
	}
	if nv(item["cache_read_input_tokens"]) != "12" {
		t.Errorf("cache_read_input_tokens = %q, want 12", nv(item["cache_read_input_tokens"]))
	}
	if !bv(item["escalated"]) {
		t.Errorf("escalated should be true")
	}
}

// A record WITHOUT the extra fields (a legacy/failure message) must not gain empty
// contract attributes — absence stays distinct from a written zero.
func TestUsageRecordContract_AbsentStayAbsent(t *testing.T) {
	var u usage
	if err := json.Unmarshal([]byte(`{"request_id":"r1","ts":"2026-01-01T00:00:00Z","status":"error","mode":"sync"}`), &u); err != nil {
		t.Fatalf("decode: %v", err)
	}
	item := buildItem(u)
	for _, k := range []string{"reasoning_chars", "thinking_budget_tokens", "pricing_status", "escalation_reason"} {
		if _, ok := item[k]; ok {
			t.Errorf("field %q must be absent when the message did not carry it", k)
		}
	}
	if sv(item["mode"]) != "sync" {
		t.Errorf("mode should round-trip as sync")
	}
}
