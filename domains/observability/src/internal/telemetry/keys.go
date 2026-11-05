// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package telemetry

// Shared Cost_Store partition literals.
//
// These existed as bare strings scattered across four Lambdas (usage-writer,
// usage-api, alert-notifier, hints-publisher). The writer moved to the single-org
// "USAGE" partition but the readers kept querying "TENANT#<org>", and the alert log
// was written under "ALERTLOG#<org>" while being read from "ALERTLOG" — so alerts
// never fired, routing hints were never published and the alert history could never
// be returned (OBS-FUN-001, OBS-FUN-002). Centralising the literals here is what makes
// the writer/reader seam a single source of truth that a test can pin, so it cannot
// drift silently again.
const (
	// UsagePartition is the one partition key under which every Usage_Record is
	// written and read. Single org per deployment (the Cost_Store dropped the tenant
	// dimension), so there is no per-org partition.
	UsagePartition = "USAGE"

	// AlertLogPartition is the partition for the alert firing history. Single-org, to
	// match UsagePartition's model: writer and reader share this one literal.
	AlertLogPartition = "ALERTLOG"
)
