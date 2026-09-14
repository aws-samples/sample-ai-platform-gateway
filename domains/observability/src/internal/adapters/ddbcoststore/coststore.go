// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbcoststore implements ports.CostStore against the Cost_Store (DynamoDB).
// It is the adapter that runs the Query by org partition and converts each item into
// the domain type (telemetry.Record). Pagination (LastEvaluatedKey) lives here —
// preserved exactly as it was in the shell.
package ddbcoststore

import (
	"context"
	"strconv"

	"github.com/aiplat/observability/internal/ports"
	"github.com/aiplat/observability/internal/telemetry"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Store reads the deployment's Cost_Store. It satisfies ports.CostStore.
type Store struct {
	ddb   *dynamodb.Client
	table string
}

// New builds the adapter with the DynamoDB client and the table name.
func New(ddb *dynamodb.Client, table string) *Store {
	return &Store{ddb: ddb, table: table}
}

// compile-time assertion: the adapter implements the port (design, R3.2/Property 7).
var _ ports.CostStore = (*Store)(nil)

// Query brings in all Usage_Records in the [from,to] range, paginating to the end.
// Post-refactor: no tenant parameter (pk = "USAGE") — single org per deployment.
// The '~' suffix on the upper bound includes the end of the range (it is
// greater than any digit/':' of an RFC3339 timestamp).
func (s *Store) Query(ctx context.Context, from, to string) ([]telemetry.Record, error) {
	return s.query(ctx, "", "USAGE", from, to)
}

// QueryApp reads one app's records through the gsi1 index, whose partition key is
// "APP#<app>" and whose range key is the same sk as the base table.
//
// The index has existed and been populated since the Cost_Store was created, and until
// now nothing read it: every per-app number in the console came from reading the whole
// time range on the single hot "USAGE" partition and grouping in memory. That works while
// a deployment is small and stops working exactly when per-app attribution starts to
// matter — the read cost of one project's report grows with every other project's traffic.
//
// projection_type = ALL on the index is what makes this a drop-in: the items come back
// whole, so the same conversion below serves both reads.
func (s *Store) QueryApp(ctx context.Context, app, from, to string) ([]telemetry.Record, error) {
	if app == "" {
		return s.Query(ctx, from, to)
	}
	// "none" is what the writer stores for a record with no app, so it is a legitimate
	// filter value and not a special case here.
	return s.query(ctx, "gsi1", "APP#"+app, from, to)
}

// query is the shared read. index == "" means the base table; the key condition is the
// same shape either way because gsi1 shares the base table's sort key.
func (s *Store) query(ctx context.Context, index, pk, from, to string) ([]telemetry.Record, error) {
	skFrom := "TS#" + from
	skTo := "TS#" + to + "~"
	keyExpr := "pk = :pk AND sk BETWEEN :a AND :b"
	if index != "" {
		keyExpr = "gsi1pk = :pk AND sk BETWEEN :a AND :b"
	}
	var out []telemetry.Record
	var lek map[string]ddbtypes.AttributeValue
	for {
		q := &dynamodb.QueryInput{
			TableName:              &s.table,
			KeyConditionExpression: awsString(keyExpr),
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk": &ddbtypes.AttributeValueMemberS{Value: pk},
				":a":  &ddbtypes.AttributeValueMemberS{Value: skFrom},
				":b":  &ddbtypes.AttributeValueMemberS{Value: skTo},
			},
			ExclusiveStartKey: lek,
		}
		if index != "" {
			q.IndexName = awsString(index)
		}
		r, err := s.ddb.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, it := range r.Items {
			out = append(out, telemetry.Record{
				Provider:            str(it["provider"]),
				Upstream:            str(it["upstream"]),
				Model:               str(it["model"]),
				RequestedModel:      str(it["requested_model"]),
				RequestedCostUSD:    num(it["requested_cost_usd"]),
				App:                 str(it["app_tag"]),
				Team:                str(it["team"]),
				Feature:             str(it["feature"]),
				TokensIn:            int(num(it["tokens_in"])),
				TokensOut:           int(num(it["tokens_out"])),
				Cost:                num(it["estimated_cost_usd"]),
				Saved:               num(it["saved_usd"]),
				CreditUSD:           num(it["credit_usd"]),
				CashUSD:             num(it["cash_usd"]),
				SavedVerified:       num(it["saved_verified_usd"]),
				SavedCounterfactual: num(it["saved_counterfactual_usd"]),
				PriceSource:         str(it["price_source"]),
				SwapClass:           str(it["swap_class"]),
				ServedModelID:       str(it["served_model_id"]),
				Canary:              boolOf(it["canary"]),
				CanaryRoute:         str(it["canary_route"]),
				Reason:              str(it["savings_reason"]),
				Latency:             int(num(it["latency_ms"])),
				CacheHit:            boolOf(it["cache_hit"]),
				Status:              str(it["status"]),
				FailReason:          str(it["reason"]),
				Detail:              str(it["detail"]),
				Category:            str(it["category"]),
				SLIEligible:         boolOf(it["sli_eligible"]),
				Mode:                modeOf(it["mode"]),
				TS:                  str(it["ts"]),
			})
		}
		if r.LastEvaluatedKey == nil {
			break
		}
		lek = r.LastEvaluatedKey
	}
	return out, nil
}

// modeOf reads the `mode` field, treating absence as `sync`: records written before
// the field existed all belong to the synchronous path.
func modeOf(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok && s.Value != "" {
		return s.Value
	}
	return "sync"
}

func num(av ddbtypes.AttributeValue) float64 {
	if n, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(n.Value, 64)
		return f
	}
	return 0
}

func str(av ddbtypes.AttributeValue) string {
	if s, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return s.Value
	}
	return ""
}

func boolOf(av ddbtypes.AttributeValue) bool {
	if b, ok := av.(*ddbtypes.AttributeValueMemberBOOL); ok {
		return b.Value
	}
	return false
}

func awsString(s string) *string { return &s }
