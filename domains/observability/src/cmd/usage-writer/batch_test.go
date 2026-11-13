// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// OBS-NFR-001: a single failing record is reported in isolation; a malformed record is
// dropped (logged), not retried; good records are not re-driven.
func TestHandle_PartialBatchFailure(t *testing.T) {
	body := func(id string) string {
		return `{"request_id":"` + id + `","ts":"2026-01-01T00:00:00Z","app_tag":"a","estimated_cost_usd":0.1}`
	}
	orig := putFn
	defer func() { putFn = orig }()
	putFn = func(_ context.Context, item map[string]ddbtypes.AttributeValue) error {
		if sv(item["request_id"]) == "boom" {
			return errors.New("throttled")
		}
		return nil
	}
	e := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "m1", Body: body("ok1")},
		{MessageId: "m2", Body: body("boom")},
		{MessageId: "m3", Body: "{not json"},
	}}
	resp, err := handle(context.Background(), e)
	if err != nil {
		t.Fatalf("handle returned error: %v", err)
	}
	if len(resp.BatchItemFailures) != 1 || resp.BatchItemFailures[0].ItemIdentifier != "m2" {
		t.Fatalf("expected only m2 reported as failed, got %+v", resp.BatchItemFailures)
	}
}
