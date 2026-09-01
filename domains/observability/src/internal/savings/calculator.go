// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package savings calculates cost savings and provides breakdowns by team/app.
// Post-refactor: no org aggregation — single deployment with team/app groupings.
package savings

import (
	"context"
	"fmt"
	"time"

	"github.com/aiplat/observability/internal/repository"
	"github.com/aiplat/observability/internal/telemetry"
)

// Calculator computes savings metrics from usage records.
type Calculator struct {
	repo repository.UsageRepository
}

// NewCalculator creates a new savings calculator.
func NewCalculator(repo repository.UsageRepository) *Calculator {
	return &Calculator{repo: repo}
}

// SavingsReport contains aggregated savings data.
type SavingsReport struct {
	Period   string
	Proven   SavingsMetrics
	Supposed SavingsMetrics
	ByTeam   map[string]TeamSavings
	ByApp    map[string]AppSavings
}

// SavingsMetrics represents a category of savings.
type SavingsMetrics struct {
	TotalUSD     float64
	CacheUSD     float64
	RoutingUSD   float64
	FallbackUSD  float64
	RequestCount int
}

// TeamSavings contains team-level savings breakdown.
type TeamSavings struct {
	Team     string
	Proven   SavingsMetrics
	Supposed SavingsMetrics
}

// AppSavings contains app-level savings breakdown.
type AppSavings struct {
	App      string
	Team     string
	Proven   SavingsMetrics
	Supposed SavingsMetrics
}

// CalculateSavings computes savings for the specified time period.
// Post-refactor: aggregates for the entire deployment, with team/app breakdowns.
func (c *Calculator) CalculateSavings(ctx context.Context, period string) (*SavingsReport, error) {
	start, end, err := parsePeriod(period)
	if err != nil {
		return nil, fmt.Errorf("invalid period: %w", err)
	}

	// Query all usage for the period
	usage, err := c.repo.QueryUsage(ctx, repository.UsageFilter{
		StartTime: start,
		EndTime:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query usage: %w", err)
	}

	// Calculate overall savings
	proven := c.calculateProven(usage)
	supposed := c.calculateSupposed(usage)

	// Group by team and app
	byTeam := c.groupByTeam(usage)
	byApp := c.groupByApp(usage)

	return &SavingsReport{
		Period:   period,
		Proven:   proven,
		Supposed: supposed,
		ByTeam:   byTeam,
		ByApp:    byApp,
	}, nil
}

// calculateProven computes verified savings (cache, observable).
func (c *Calculator) calculateProven(usage []telemetry.Record) SavingsMetrics {
	metrics := SavingsMetrics{}

	for _, r := range usage {
		if r.SavedVerified > 0 {
			metrics.TotalUSD += r.SavedVerified
			metrics.RequestCount++

			switch r.Reason {
			case "cache", "provider_prompt_cache":
				metrics.CacheUSD += r.SavedVerified
			case "provider_arbitrage":
				metrics.RoutingUSD += r.SavedVerified
			}
		}
	}

	return metrics
}

// calculateSupposed computes counterfactual savings (model swaps, assumed baseline).
func (c *Calculator) calculateSupposed(usage []telemetry.Record) SavingsMetrics {
	metrics := SavingsMetrics{}

	for _, r := range usage {
		if r.SavedCounterfactual > 0 {
			metrics.TotalUSD += r.SavedCounterfactual
			metrics.RequestCount++

			switch r.Reason {
			case "auto_cheapest":
				metrics.RoutingUSD += r.SavedCounterfactual
			case "fallback", "budget_degrade":
				metrics.FallbackUSD += r.SavedCounterfactual
			}
		}
	}

	return metrics
}

// groupByTeam aggregates savings by team.
func (c *Calculator) groupByTeam(usage []telemetry.Record) map[string]TeamSavings {
	teams := make(map[string][]telemetry.Record)

	for _, r := range usage {
		team := r.Team
		if team == "" {
			team = "default"
		}
		teams[team] = append(teams[team], r)
	}

	result := make(map[string]TeamSavings)
	for team, recs := range teams {
		result[team] = TeamSavings{
			Team:     team,
			Proven:   c.calculateProven(recs),
			Supposed: c.calculateSupposed(recs),
		}
	}

	return result
}

// groupByApp aggregates savings by app.
func (c *Calculator) groupByApp(usage []telemetry.Record) map[string]AppSavings {
	apps := make(map[string][]telemetry.Record)

	for _, r := range usage {
		app := r.App
		if app == "" {
			app = "none"
		}
		apps[app] = append(apps[app], r)
	}

	result := make(map[string]AppSavings)
	for app, recs := range apps {
		team := "default"
		if len(recs) > 0 {
			team = recs[0].Team
		}

		result[app] = AppSavings{
			App:      app,
			Team:     team,
			Proven:   c.calculateProven(recs),
			Supposed: c.calculateSupposed(recs),
		}
	}

	return result
}

// parsePeriod converts a period string to start/end times.
// Supports: "7d", "30d", "90d", "MTD", "YTD", "2024-01", "2024-Q1"
func parsePeriod(period string) (start, end time.Time, err error) {
	now := time.Now().UTC()
	end = now

	switch period {
	case "7d":
		start = now.AddDate(0, 0, -7)
	case "30d":
		start = now.AddDate(0, 0, -30)
	case "90d":
		start = now.AddDate(0, 0, -90)
	case "MTD":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "YTD":
		start = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		// Try parsing as date range
		var t time.Time
		t, err = time.Parse("2006-01", period)
		if err == nil {
			start = t
			end = start.AddDate(0, 1, 0).Add(-time.Second)
		} else {
			err = fmt.Errorf("unsupported period format: %s", period)
		}
	}

	return start, end, err
}
