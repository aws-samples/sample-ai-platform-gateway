// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbconfig is the outbound adapter for the Core's effective config, read
// from the Governance config table as a CONTRACT (never a synchronous Lambda call).
//
// Feature: hexagonal-refactor, task 4.2. Code MOVED from cmd/router/main.go
// (loadConfig, scopeKeys, deepMerge and the 15s cache) without rewriting the logic.
// It preserves the mandatory FALLBACK: unavailable config does not take the gateway
// down — the caller passes the environment defaults as `base`, and whatever the
// table brings is merged on top (the most specific wins). An empty table ⇒ returns
// only the defaults. Converting the effective map into the Config type (the
// handler's) stays with the caller: this package is infrastructure and does not
// know the decision type.
package ddbconfig

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

const cacheTTL = 15 * time.Second

type entry struct {
	merged      map[string]interface{}
	budgetScope string
	limitsScope string
	at          time.Time
}

// Store is the production adapter over the config table, with a short cache.
type Store struct {
	ddb   *dynamodb.Client
	table string
	// org is this deployment's single org (see scopeKeys) — never empty in
	// production; New falls back to "default" so a missing env var degrades to
	// a stable (if wrong) scope instead of silently reading nothing forever.
	org string

	mu    sync.Mutex
	cache map[string]entry
}

var _ ports.ConfigStore = (*Store)(nil) // compile-time assertion

// New builds the adapter with the client, the table name (may be "") and the
// deployment's org (may be ""; falls back to "default").
func New(ddb *dynamodb.Client, table string, org string) *Store {
	if org == "" {
		org = "default"
	}
	return &Store{ddb: ddb, table: table, org: org, cache: map[string]entry{}}
}

// Reset clears the cache. Used in tests to isolate scenarios.
func (s *Store) Reset() {
	s.mu.Lock()
	s.cache = map[string]entry{}
	s.mu.Unlock()
}

// scopeKeys returns the config keys from the least to the most specific.
//
// Single-org model: the deployment has exactly one org (never chosen per
// request), but the config table is STILL partitioned by org — Governance
// writes every scope under ORG#<org>#... (see govcore.ScopeKeys) because it is
// the shared control-plane schema across domains. The Core must read the SAME
// keys it does not write, or every team/app-level setting configured through
// the console (model catalog, budget, rate limits, cache) silently never
// reaches the gateway. `org` here is the single deployment org (DEPLOYMENT_ORG
// env var / Contract of Environment), not a per-request parameter.
func scopeKeys(org, team, app string) []string {
	keys := []string{"global"}
	if org == "" {
		return keys
	}
	keys = append(keys, "ORG#"+org)
	if team == "" {
		team = "default"
	}
	keys = append(keys, "ORG#"+org+"#TEAM#"+team)
	if app != "" {
		keys = append(keys, "ORG#"+org+"#TEAM#"+team+"#APP#"+app)
	}
	return keys
}

// deepMerge overlays src onto dst: maps are merged key by key, scalars/lists are
// replaced. That is what lets an org add a model without repeating the catalog.
func deepMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		if sv, ok := v.(map[string]interface{}); ok {
			if dv, ok2 := dst[k].(map[string]interface{}); ok2 {
				deepMerge(dv, sv)
				continue
			}
			cp := map[string]interface{}{}
			deepMerge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// Effective resolves the EFFECTIVE config for team/app: it starts from `base`
// (environment defaults, supplied by the caller) and merges global → TEAM → APP.
// It returns the merged map and the scopes that DEFINED budget and rate_limits
// (the enforcement counter uses that scope). 15s cache per team|app.
//
// Single-org model: org is the deployment's own (s.org), never a parameter — a
// request only ever chooses team/app.
// IMPORTANT: `base` is mutated (the merge writes into it). The caller must pass its
// own map per call — as the original loadConfig did.
func (s *Store) Effective(ctx context.Context, team, app string, base map[string]interface{}) (map[string]interface{}, string, string) {
	ck := team + "|" + app
	s.mu.Lock()
	if e, ok := s.cache[ck]; ok && time.Since(e.at) < cacheTTL {
		s.mu.Unlock()
		return e.merged, e.budgetScope, e.limitsScope
	}
	s.mu.Unlock()

	budgetScope, limitsScope := "", ""

	if s.table != "" {
		keys := scopeKeys(s.org, team, app)
		reqKeys := make([]map[string]ddbtypes.AttributeValue, 0, len(keys))
		for _, k := range keys {
			reqKeys = append(reqKeys, map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: k}})
		}
		out, err := s.ddb.BatchGetItem(ctx, &dynamodb.BatchGetItemInput{
			RequestItems: map[string]ddbtypes.KeysAndAttributes{s.table: {Keys: reqKeys}},
		})
		if err == nil {
			// Index the result (Batch does not guarantee order) and merge in the right order.
			byPk := map[string]map[string]interface{}{}
			for _, it := range out.Responses[s.table] {
				pk, _ := it["pk"].(*ddbtypes.AttributeValueMemberS)
				cfgv, _ := it["config"].(*ddbtypes.AttributeValueMemberS)
				if pk == nil || cfgv == nil {
					continue
				}
				var m map[string]interface{}
				if json.Unmarshal([]byte(cfgv.Value), &m) == nil {
					byPk[pk.Value] = m
				}
			}
			for _, k := range keys {
				if m, ok := byPk[k]; ok {
					deepMerge(base, m)
					if _, has := m["budget"]; has {
						budgetScope = k
					}
					if _, has := m["rate_limits"]; has {
						limitsScope = k
					}
				}
			}
		}
	}

	s.mu.Lock()
	s.cache[ck] = entry{merged: base, budgetScope: budgetScope, limitsScope: limitsScope, at: time.Now()}
	s.mu.Unlock()
	return base, budgetScope, limitsScope
}
