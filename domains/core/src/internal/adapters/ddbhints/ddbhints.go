// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbhints reads the Routing_Hints published by Observability.
//
// It is an ARTIFACT read used as a contract — never a call to another domain's
// Lambda or API (the golden rule in `aiplat-domains.md`). If the artifact is
// missing, unreadable, of an unknown version or too old, it returns nil and the
// domain falls back to the heuristic: unavailable hints must not degrade the gateway.
package ddbhints

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/routing"
)

// Contract version this reader understands. A different version = ignore, rather
// than misinterpreting an artifact in another format.
const supportedVersion = 1

// maxAge: an artifact older than this is not trustworthy to predict anything. The
// publisher runs hourly, so 6h means it is broken.
const maxAge = 6 * time.Hour

// A short cacheTTL, same as the config's: the decision tolerates 15s of staleness,
// and a GetItem per request would be expensive with no gain.
const cacheTTL = 15 * time.Second

type entry struct {
	h  *routing.Hints
	at time.Time
}

// Reader reads hints with a local cache per (org, feature).
type Reader struct {
	ddb   *dynamodb.Client
	table string

	mu    sync.Mutex
	cache map[string]entry
}

func New(ddb *dynamodb.Client, table string) *Reader {
	return &Reader{ddb: ddb, table: table, cache: map[string]entry{}}
}

// artifact is the format published by the hints-publisher.
type artifact struct {
	Version      int            `json:"version"`
	GeneratedAt  string         `json:"generated_at"`
	WindowDays   int            `json:"window_days"`
	Samples      int            `json:"samples"`
	MedianOut    map[string]int `json:"median_out_by_model"`
	SamplesByKey map[string]int `json:"samples_by_key"`
	Unavailable  map[string]int `json:"unavailable_until_unix"`
}

// Hints returns the org/feature hints, or nil when there is no usable data.
//
// nil is NOT an error: it is the normal path for a new org, a new feature or a
// publisher that is down. The domain treats nil as "use the heuristic".
func (r *Reader) Hints(ctx context.Context, org, feature string, now time.Time) *routing.Hints {
	if r == nil || r.table == "" || org == "" {
		return nil
	}
	key := org + "|" + feature

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && now.Sub(e.at) < cacheTTL {
		r.mu.Unlock()
		return e.h
	}
	r.mu.Unlock()

	h := r.fetch(ctx, org, feature, now)

	r.mu.Lock()
	r.cache[key] = entry{h: h, at: now}
	r.mu.Unlock()
	return h
}

func (r *Reader) fetch(ctx context.Context, org, feature string, now time.Time) *routing.Hints {
	// The feature item first; the org aggregate as a fallback. A GetItem on a small
	// item instead of a Query pulling everything — that is what fits the latency budget.
	if feature != "" {
		if h := r.get(ctx, org, "HINTS#v1#"+feature, now); h != nil {
			return h
		}
	}
	return r.get(ctx, org, "HINTS#v1#*", now)
}

func (r *Reader) get(ctx context.Context, org, sk string, now time.Time) *routing.Hints {
	out, err := r.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(r.table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: "ORG#" + org},
			"sk": &ddbtypes.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil || out.Item == nil {
		return nil
	}
	raw, ok := out.Item["hints"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil
	}
	var a artifact
	if json.Unmarshal([]byte(raw.Value), &a) != nil {
		return nil
	}
	if a.Version != supportedVersion {
		return nil // unknown version: ignore instead of misinterpreting
	}
	gen, err := time.Parse(time.RFC3339, a.GeneratedAt)
	if err != nil || now.Sub(gen) > maxAge {
		return nil // stale data is worse than an honest heuristic
	}

	unav := map[string]time.Time{}
	for k, ts := range a.Unavailable {
		unav[k] = time.Unix(int64(ts), 0).UTC()
	}
	return &routing.Hints{
		Version:      a.Version,
		GeneratedAt:  gen,
		Samples:      a.Samples,
		MedianOut:    a.MedianOut,
		SamplesByKey: a.SamplesByKey,
		Unavailable:  unav,
	}
}
