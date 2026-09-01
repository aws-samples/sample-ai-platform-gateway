// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package repository provides storage abstractions for the Observability domain,
// enabling database-agnostic usage tracking. After the multi-org removal, records
// no longer partition by org_id — they are scoped to the single deployment.
package repository

import (
	"context"
	"time"

	"github.com/aiplat/observability/internal/telemetry"
)

// UsageFilter specifies criteria for querying usage records.
type UsageFilter struct {
	StartTime time.Time
	EndTime   time.Time
	Team      string
	App       string
	Feature   string
	Model     string
	Provider  string
}

// UsageRepository handles persistence and retrieval of usage records.
// Post-refactor: no org_id parameter — single org per deployment.
type UsageRepository interface {
	// RecordUsage persists a usage record. Idempotent by request_id + timestamp.
	RecordUsage(ctx context.Context, record *UsageRecord) error

	// QueryUsage retrieves usage records matching the filter.
	QueryUsage(ctx context.Context, filter UsageFilter) ([]telemetry.Record, error)
}

// UsageRecord is the domain model for a single API request's usage metrics.
// No OrgID field — single org per deployment. Team/App/Feature are preserved
// for cost allocation and granular tracking.
type UsageRecord struct {
	RequestID           string
	Team                string // Kept for cost allocation
	App                 string // Kept for cost allocation
	Feature             string // Kept for granular tracking
	Provider            string
	Upstream            string
	Model               string
	RequestedModel      string
	RequestedCostUSD    float64
	TokensIn            int
	TokensOut           int
	Cost                float64
	Saved               float64
	SavingsReason       string
	SavedVerified       float64
	SavedCounterfactual float64
	SavingsClass        string
	LatencyMs           int
	CacheHit            bool
	Status              string
	Reason              string
	Detail              string
	Category            string
	SLIEligible         bool
	PaidFrom            string
	CreditUSD           float64
	CashUSD             float64
	PriceSource         string
	SwapClass           string
	ServedModelID       string
	Canary              bool
	CanaryRoute         string
	Timestamp           time.Time
	Mode                string
}
