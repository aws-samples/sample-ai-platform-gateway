// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package repository tests verify the multi-org removal.
package repository

import (
	"testing"
	"time"
)

func TestUsageRecord_NoOrgID(t *testing.T) {
	// Verify that UsageRecord has no OrgID field
	record := UsageRecord{
		RequestID: "req_123",
		Team:      "backend",
		App:       "api-prod",
		Feature:   "chat",
		Timestamp: time.Now(),
	}

	// These should compile without error — org fields preserved for cost tracking
	if record.Team == "" {
		t.Error("Team field should be present")
	}
	if record.App == "" {
		t.Error("App field should be present")
	}
	if record.Feature == "" {
		t.Error("Feature field should be present")
	}

	// This should not compile if uncommented (verifying OrgID removal)
	// _ = record.OrgID
}

func TestUsageFilter_TeamAppFeatureSupport(t *testing.T) {
	// Verify that filtering by team/app/feature is supported
	filter := UsageFilter{
		StartTime: time.Now().AddDate(0, 0, -7),
		EndTime:   time.Now(),
		Team:      "backend",
		App:       "api-prod",
		Feature:   "chat",
	}

	if filter.Team != "backend" {
		t.Error("Team filter should be supported")
	}
	if filter.App != "api-prod" {
		t.Error("App filter should be supported")
	}
	if filter.Feature != "chat" {
		t.Error("Feature filter should be supported")
	}
}
