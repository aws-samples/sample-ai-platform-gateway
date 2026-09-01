// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddblimits is the outbound adapter for the atomic enforcement counters
// (rate limit, monthly spend, credit) in the Core's *-limits table, with TTL.
//
// Feature: hexagonal-refactor, task 4.4. Code MOVED from cmd/router/main.go
// (bump, readCounter) without rewriting the logic. The policies that USE these
// counters (checkRate, addSpend, addRateTokens, addCreditSpend, readSpend) stay in
// the handler — they are orchestration, not table mechanics. It preserves the no-op
// when the table is not configured.
package ddblimits

import (
	"context"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

// Store is the production adapter over the limits table.
type Store struct {
	ddb   *dynamodb.Client
	table string
}

var _ ports.LimitsStore = (*Store)(nil) // compile-time assertion

// New builds the adapter with the client and the table name (may be "").
func New(ddb *dynamodb.Client, table string) *Store {
	return &Store{ddb: ddb, table: table}
}

// Bump atomically increments a counter and returns the new value.
func (s *Store) Bump(ctx context.Context, pk, field string, delta float64, ttl time.Time) (float64, error) {
	if s.table == "" {
		return 0, nil
	}
	upd := "ADD " + field + " :d SET expires_at = :e"
	out, err := s.ddb.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        &s.table,
		Key:              map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
		UpdateExpression: &upd,
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":d": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(delta, 'f', -1, 64)},
			":e": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(ttl.Unix(), 10)},
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	if v, ok := out.Attributes[field].(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(v.Value, 64)
		return f, nil
	}
	return 0, nil
}

// Read returns a counter's current value (0 on error/absence).
func (s *Store) Read(ctx context.Context, pk, field string) float64 {
	if s.table == "" {
		return 0
	}
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table,
		Key:       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
	})
	if err != nil || out.Item == nil {
		return 0
	}
	if v, ok := out.Item[field].(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(v.Value, 64)
		return f
	}
	return 0
}
