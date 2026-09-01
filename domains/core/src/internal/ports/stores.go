// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package ports

import (
	"context"
	"time"
)

// The interfaces below are DECLARED in Phase 0 so the boundary is explicit
// (Req 7.4), but the implementations stay inline in cmd/router until the later
// adapter migration task. Req 7.9 requires preserving the observable behavior of
// auth, cache, rate limit, budget and fallback — hence the migration is its own
// commit, with characterization tests before moving budget/rate limit.

// ConfigSource delivers the effective config for a scope (global → org → team → app).
// The consumer needs a FALLBACK: unavailable config must not take the gateway down.
type ConfigSource interface {
	Effective(ctx context.Context, org, team, app string) (map[string]interface{}, error)
}

// HintSource delivers the Routing_Hints artifact published by Observability.
// Asynchronous contract: an item read, never a call to a Lambda or API of another
// domain. Returning (nil, nil) is valid and means "no hints" → heuristic.
type HintSource interface {
	Hints(ctx context.Context, org, feature string) ([]byte, error)
}

// UsageSink publishes the Usage_Record. It is fire-and-forget on purpose: the
// response to the client never waits for the usage emission.
type UsageSink interface {
	Emit(ctx context.Context, rec map[string]interface{})
}

// LimitsStore is the atomic enforcement counter (rate limit, spend, credit).
type LimitsStore interface {
	Bump(ctx context.Context, pk, field string, delta float64, expires time.Time) (float64, error)
	Read(ctx context.Context, pk, field string) float64
}

// ConfigStore delivers the already merged effective config (global → TEAM → APP).
// `base` holds the environment defaults provided by the caller (fallback): the
// adapter merges whatever the table brings on top and returns the effective map
// plus the scopes that defined budget and rate_limits. Unavailable config ⇒
// returns `base` (the gateway keeps serving) — the degradation is contract (R3.3).
//
// Single-org model: org parameter removed. Hierarchy is now: global → team → app.
type ConfigStore interface {
	Effective(ctx context.Context, team, app string, base map[string]interface{}) (map[string]interface{}, string, string)
}

// Cache is the response cache port. Get returns the stored JSON and the cost of
// the cached response; ok=false on miss, error or cache disabled. The degradation
// is contract: an error NEVER takes the request down (R3.3).
type Cache interface {
	Get(ctx context.Context, key string) (respJSON string, cost float64, ok bool)
	Put(ctx context.Context, key, provider, respJSON string, cost float64, ttlSeconds int)
	Enabled() bool
}

// SecretStore resolves provider credentials from the deployment's secret store.
// "" when absent or on error — the consumer treats it as no key.
// Single-org model: org parameter removed.
type SecretStore interface {
	Get(ctx context.Context, name string) string
}

// KeyIdentity is the team/app hierarchy resolved from the API key.
// Org field removed for single-org deployments - org is deployment-level, not key-level.
type KeyIdentity struct{ Team, App string }

// KeyStore resolves the API key (already extracted from the header) into team/app.
// Org is omitted - single org per deployment.
//   - err != nil  → PLATFORM failure (backend unavailable) → 503, counts toward the SLI.
//   - ok == false with err == nil → invalid/missing/revoked key → 401.
type KeyStore interface {
	Resolve(ctx context.Context, key string) (KeyIdentity, bool, error)
	Enabled() bool
}
