// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package savings tests verify savings calculations without org aggregation.
package savings

import (
	"context"
	"testing"

	"github.com/aiplat/observability/internal/repository"
	"github.com/aiplat/observability/internal/telemetry"
)

// mockUsageRepository is a test double for UsageRepository.
type mockUsageRepository struct {
	records []telemetry.Record
}

func (m *mockUsageRepository) RecordUsage(ctx context.Context, record *repository.UsageRecord) error {
	return nil
}

func (m *mockUsageRepository) QueryUsage(ctx context.Context, filter repository.UsageFilter) ([]telemetry.Record, error) {
	return m.records, nil
}

func TestCalculateSavings_TeamBreakdown(t *testing.T) {
	repo := &mockUsageRepository{
		records: []telemetry.Record{
			{
				Team:          "backend",
				App:           "api-prod",
				SavedVerified: 10.0,
				Reason:        "cache",
			},
			{
				Team:                "frontend",
				App:                 "web-app",
				SavedCounterfactual: 5.0,
				Reason:              "auto_cheapest",
			},
			{
				Team:          "backend",
				App:           "api-staging",
				SavedVerified: 3.0,
				Reason:        "cache",
			},
		},
	}

	calc := NewCalculator(repo)
	report, err := calc.CalculateSavings(context.Background(), "30d")
	if err != nil {
		t.Fatalf("CalculateSavings failed: %v", err)
	}

	// Verify overall proven savings
	if report.Proven.TotalUSD != 13.0 {
		t.Errorf("Expected proven total 13.0, got %f", report.Proven.TotalUSD)
	}

	// Verify team breakdown
	if len(report.ByTeam) != 2 {
		t.Errorf("Expected 2 teams, got %d", len(report.ByTeam))
	}

	backendSavings, ok := report.ByTeam["backend"]
	if !ok {
		t.Error("backend team not found in breakdown")
	}
	if backendSavings.Proven.TotalUSD != 13.0 {
		t.Errorf("Expected backend proven 13.0, got %f", backendSavings.Proven.TotalUSD)
	}

	frontendSavings, ok := report.ByTeam["frontend"]
	if !ok {
		t.Error("frontend team not found in breakdown")
	}
	if frontendSavings.Supposed.TotalUSD != 5.0 {
		t.Errorf("Expected frontend supposed 5.0, got %f", frontendSavings.Supposed.TotalUSD)
	}
}

func TestCalculateSavings_AppBreakdown(t *testing.T) {
	repo := &mockUsageRepository{
		records: []telemetry.Record{
			{
				Team:          "backend",
				App:           "api-prod",
				SavedVerified: 10.0,
				Reason:        "cache",
			},
			{
				Team:          "backend",
				App:           "api-staging",
				SavedVerified: 3.0,
				Reason:        "cache",
			},
		},
	}

	calc := NewCalculator(repo)
	report, err := calc.CalculateSavings(context.Background(), "30d")
	if err != nil {
		t.Fatalf("CalculateSavings failed: %v", err)
	}

	// Verify app breakdown
	if len(report.ByApp) != 2 {
		t.Errorf("Expected 2 apps, got %d", len(report.ByApp))
	}

	apiProdSavings, ok := report.ByApp["api-prod"]
	if !ok {
		t.Error("api-prod app not found in breakdown")
	}
	if apiProdSavings.Proven.TotalUSD != 10.0 {
		t.Errorf("Expected api-prod proven 10.0, got %f", apiProdSavings.Proven.TotalUSD)
	}
	if apiProdSavings.Team != "backend" {
		t.Errorf("Expected api-prod team backend, got %s", apiProdSavings.Team)
	}
}

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		period      string
		shouldError bool
	}{
		{"7d", false},
		{"30d", false},
		{"90d", false},
		{"MTD", false},
		{"YTD", false},
		{"2024-01", false},
		{"invalid", true},
	}

	for _, tt := range tests {
		t.Run(tt.period, func(t *testing.T) {
			start, end, err := parsePeriod(tt.period)
			if tt.shouldError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.shouldError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if !tt.shouldError && !start.Before(end) {
				t.Error("Start should be before end")
			}
		})
	}
}

func TestCalculateSavings_NoOrgAggregation(t *testing.T) {
	// This test verifies that there's no org-level aggregation
	// All savings are at the deployment level with team/app breakdowns

	repo := &mockUsageRepository{
		records: []telemetry.Record{
			{Team: "team1", App: "app1", SavedVerified: 10.0},
			{Team: "team2", App: "app2", SavedVerified: 20.0},
		},
	}

	calc := NewCalculator(repo)
	report, err := calc.CalculateSavings(context.Background(), "30d")
	if err != nil {
		t.Fatalf("CalculateSavings failed: %v", err)
	}

	// Report should have deployment-level totals (no org field)
	if report.Period == "" {
		t.Error("Period should be set")
	}

	// Verify total is sum across all teams (deployment-wide)
	if report.Proven.TotalUSD != 30.0 {
		t.Errorf("Expected deployment total 30.0, got %f", report.Proven.TotalUSD)
	}

	// Verify team breakdowns exist
	if len(report.ByTeam) != 2 {
		t.Errorf("Expected 2 teams, got %d", len(report.ByTeam))
	}
}
