// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ports defines the OUTBOUND boundaries of the Audit domain.
//
// A port only exists here when there is a reason: runtime substitution, a double to
// test orchestration, or dependency inversion. TrailStore exists because the writer
// and the api need a double in tests (you do not test idempotency or the gate
// against a real DynamoDB).
package ports

import (
	"context"

	"github.com/aiplat/audit/internal/auditcore"
)

// TrailQuery is the slice requested from the store. The fields are combinable
// filters; picking an index is the ADAPTER's responsibility, not the caller's — the
// shell asks for "category X in period Y" and does not know an LSI exists.
type TrailQuery struct {
	Org      string
	Category string // filter by category (uses the by_category LSI)
	Actor    string // filter by actor (uses the by_actor LSI)
	Action   string // post-slice refinement
	Target   string // post-slice refinement
	FromTS   string // RFC3339, inclusive
	ToTS     string // RFC3339, inclusive
	Limit    int
	Token    string // opaque pagination token (empty = first page)
}

// TrailStore is the persisted trail. There is NO update and NO delete method: that
// absence is part of the append-only guarantee (Property 3) — what is not on the
// port cannot be called by mistake from the shell.
type TrailStore interface {
	// Append writes a record. It must be IDEMPOTENT by event_id: reprocessing the
	// same message (SQS delivery is at-least-once) must not create a second record
	// nor return an error.
	Append(ctx context.Context, ev auditcore.Event, expiresAt int64) error

	// Query returns the records in the slice, in descending order of instant, plus
	// the token for the next page (empty when there are no more).
	Query(ctx context.Context, q TrailQuery) ([]auditcore.Event, string, error)
}
