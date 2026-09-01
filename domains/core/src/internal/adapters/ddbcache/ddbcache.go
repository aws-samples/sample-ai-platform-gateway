// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbcache is the outbound adapter for the response cache (DynamoDB, TTL).
//
// Feature: hexagonal-refactor, task 4.3. Code MOVED from cmd/router/main.go
// (the inline cache read/write) without rewriting the logic. It preserves the
// degradation: an unavailable cache NEVER takes the request down — Get returns
// ok=false on error, and Put ignores failures. A disabled cache (table "") is a no-op.
//
// The cacheKey (which includes the org, structural isolation) is still computed in
// the handler: it depends on chatMsg, a handler type, not on the table.
package ddbcache

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

// Store is the production adapter over the cache table.
type Store struct {
	ddb   *dynamodb.Client
	table string
}

var _ ports.Cache = (*Store)(nil) // compile-time assertion

// New builds the adapter with the client and the table name (may be "").
func New(ddb *dynamodb.Client, table string) *Store {
	return &Store{ddb: ddb, table: table}
}

// Enabled tells whether the cache is configured.
func (s *Store) Enabled() bool { return s.table != "" }

// Get returns (response_json, cost_usd, ok). ok=false on miss, error or when disabled.
func (s *Store) Get(ctx context.Context, key string) (respJSON string, cost float64, ok bool) {
	if s.table == "" {
		return "", 0, false
	}
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table,
		Key:       map[string]ddbtypes.AttributeValue{"cache_key": &ddbtypes.AttributeValueMemberS{Value: key}},
	})
	if err != nil || out.Item == nil {
		return "", 0, false
	}
	v, okv := out.Item["response_json"].(*ddbtypes.AttributeValueMemberS)
	if !okv {
		return "", 0, false
	}
	if cv, okc := out.Item["cost_usd"].(*ddbtypes.AttributeValueMemberN); okc {
		cost, _ = strconv.ParseFloat(cv.Value, 64)
	}
	return v.Value, cost, true
}

// Put stores the response with a TTL (seconds from now). Best-effort.
func (s *Store) Put(ctx context.Context, key, provider, respJSON string, cost float64, ttlSeconds int) {
	if s.table == "" {
		return
	}
	ttl := strconv.FormatInt(time.Now().Unix()+int64(ttlSeconds), 10)
	s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: map[string]ddbtypes.AttributeValue{
		"cache_key":     &ddbtypes.AttributeValueMemberS{Value: key},
		"provider":      &ddbtypes.AttributeValueMemberS{Value: provider},
		"response_json": &ddbtypes.AttributeValueMemberS{Value: respJSON},
		"cost_usd":      &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(cost, 'f', -1, 64)},
		"expires_at":    &ddbtypes.AttributeValueMemberN{Value: ttl},
	}})
}

// --- Semantic cache index -----------------------------------------------------
//
// Design done "our way": instead of a dedicated vector store (FAISS/Milvus), each
// org's vector index is ONE item in this same table, keyed SEMIDX#<org>. A read is
// 1 GetItem; a write is 1 PutItem (the handler reuses the index already loaded on
// the read). No GSI, no new table, no new IAM — the router policy already covers
// GetItem/PutItem on this table. The search (cosine) runs in the Lambda over the
// org's set, which is small and expires on its own (TTL).
//
// The vector is stored QUANTIZED (int8 base64 + scale): ~4x smaller than float, so
// hundreds of entries fit in one item well below DynamoDB's 400KB limit.

func semIndexKey(org string) string { return "SEMIDX#" + org }

// SemEntry is one index entry: the response cache key plus the quantized vector of
// the question that produced it. Short tags to save bytes in the item.
//
// Ctx is the context fingerprint (system prompt + model). It partitions the index so
// that different personas and models never share a response. An entry with an EMPTY
// Ctx predates the false-positive fix — its vector was computed over the whole
// system prompt and is unusable, so the reader IGNORES it.
type SemEntry struct {
	CacheKey string  `json:"k"`
	Q        string  `json:"q"` // base64 of the quantized vector's int8 bytes
	Scale    float64 `json:"s"`
	Ctx      string  `json:"c,omitempty"`
	Num      string  `json:"n,omitempty"`
}

// GetSemIndex reads the org's semantic index. Absence/error returns an empty list
// (degradation: with no index the handler simply MISSes). It never propagates an
// error that would take the request down.
func (s *Store) GetSemIndex(ctx context.Context, org string) []SemEntry {
	if s.table == "" {
		return nil
	}
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table,
		Key:       map[string]ddbtypes.AttributeValue{"cache_key": &ddbtypes.AttributeValueMemberS{Value: semIndexKey(org)}},
	})
	if err != nil || out.Item == nil {
		return nil
	}
	v, ok := out.Item["idx"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil
	}
	var entries []SemEntry
	if json.Unmarshal([]byte(v.Value), &entries) != nil {
		return nil
	}
	return entries
}

// PutSemIndex stores the org's index (best-effort). The TTL is aligned with the
// cache: the index expires together with it, avoiding serving a vector whose
// response is already gone.
func (s *Store) PutSemIndex(ctx context.Context, org string, entries []SemEntry, ttlSeconds int) {
	if s.table == "" {
		return
	}
	b, _ := json.Marshal(entries)
	ttl := strconv.FormatInt(time.Now().Unix()+int64(ttlSeconds), 10)
	s.ddb.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: map[string]ddbtypes.AttributeValue{
		"cache_key":  &ddbtypes.AttributeValueMemberS{Value: semIndexKey(org)},
		"idx":        &ddbtypes.AttributeValueMemberS{Value: string(b)},
		"expires_at": &ddbtypes.AttributeValueMemberN{Value: ttl},
	}})
}
