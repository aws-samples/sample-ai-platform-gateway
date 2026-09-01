// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ports declares the Observability outbound (driven) ports. It follows the
// same criterion as the Core (design, D2): a port earns its place if it needs a test
// double to exercise orchestration, runtime substitution, or dependency inversion to
// keep the pure domain compiling without the SDK.
//
// CostStore is the domain's only port: reading the Cost_Store by time range.
// Post-refactor: no org/tenant parameter — single org per deployment.
// Returns records already in the domain type (telemetry.Record) — the adapter
// (adapters/ddbcoststore) is what converts the DynamoDB item. ports imports telemetry
// (not the other way around), so there is no cycle and the pure domain stays
// independent of the port.
package ports

import (
	"context"

	"github.com/aiplat/observability/internal/telemetry"
)

// CostStore reads persisted Usage_Records in the [from,to] range.
// Post-refactor: no tenant parameter (pk = "USAGE") — single org per deployment.
// DynamoDB pagination is the adapter's responsibility: the port returns the complete set.
type CostStore interface {
	Query(ctx context.Context, from, to string) ([]telemetry.Record, error)
}
