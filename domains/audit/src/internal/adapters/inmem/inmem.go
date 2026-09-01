// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package inmem holds an in-memory double of ports.TrailStore, used to test the
// orchestration of the writer and the api without a real DynamoDB.
//
// The store double deliberately reproduces two DynamoDB characteristics the tests
// need to exercise: the CONDITIONAL write by key (idempotency) and the DESCENDING
// read order. A double that ignored those would pass tests the real adapter would fail.
package inmem

import (
	"context"
	"sort"
	"strings"

	"github.com/aiplat/audit/internal/auditcore"
	"github.com/aiplat/audit/internal/ports"
)

var _ ports.TrailStore = (*Trail)(nil)

// Trail is an in-memory TrailStore.
type Trail struct {
	// Items indexed by "pk|sk" to reproduce DynamoDB's key uniqueness.
	Items map[string]auditcore.Event
	TTLs  map[string]int64
	// AppendErr, when not nil, is returned by Append — to test the writer's failure
	// path (retry/DLQ).
	AppendErr error
	// Appends counts the EFFECTIVE writes (it does not count the rejected duplicate).
	Appends int
}

func NewTrail() *Trail {
	return &Trail{Items: map[string]auditcore.Event{}, TTLs: map[string]int64{}}
}

func (t *Trail) key(ev auditcore.Event) string {
	sk, _, _ := auditcore.SortKeys(ev.TS, ev.EventID, ev.Category, ev.Actor.Email)
	return auditcore.PartitionKey(ev.Org) + "|" + sk
}

func (t *Trail) Append(ctx context.Context, ev auditcore.Event, expiresAt int64) error {
	if t.AppendErr != nil {
		return t.AppendErr
	}
	k := t.key(ev)
	if _, exists := t.Items[k]; exists {
		return nil // already recorded: idempotent, not an error
	}
	t.Items[k] = ev
	t.TTLs[k] = expiresAt
	t.Appends++
	return nil
}

func (t *Trail) Query(ctx context.Context, q ports.TrailQuery) ([]auditcore.Event, string, error) {
	pk := auditcore.PartitionKey(q.Org)
	var out []auditcore.Event
	for k, ev := range t.Items {
		if !strings.HasPrefix(k, pk+"|") {
			continue // isolation: another org's partition never gets in
		}
		if q.Category != "" && ev.Category != q.Category {
			continue
		}
		if q.Actor != "" && !strings.EqualFold(ev.Actor.Email, q.Actor) {
			continue
		}
		if q.Action != "" && ev.Action != q.Action {
			continue
		}
		if q.Target != "" && ev.Target != q.Target {
			continue
		}
		if q.FromTS != "" && ev.TS < q.FromTS {
			continue
		}
		if q.ToTS != "" && ev.TS > q.ToTS {
			continue
		}
		out = append(out, ev)
	}
	// Descending by instant, breaking ties by event_id so it is deterministic.
	sort.Slice(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS > out[j].TS
		}
		return out[i].EventID > out[j].EventID
	})
	if q.Limit > 0 && len(out) > q.Limit {
		return out[:q.Limit], "mais", nil
	}
	return out, "", nil
}

// TTLOf exposes the stored TTL, so the test can check the retention calculation.
func (t *Trail) TTLOf(ev auditcore.Event) int64 { return t.TTLs[t.key(ev)] }
