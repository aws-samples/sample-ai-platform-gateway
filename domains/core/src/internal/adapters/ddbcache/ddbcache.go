// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbcache is the outbound adapter for the response cache (DynamoDB, TTL).
//
// (the inline cache read/write) without rewriting the logic. It preserves the
// degradation: an unavailable cache NEVER takes the request down — Get returns
// ok=false on error, and Put ignores failures. A disabled cache (table "") is a no-op.
//
// The cacheKey (which includes the org, structural isolation) is still computed in
// the handler: it depends on chatMsg, a handler type, not on the table.
package ddbcache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
	"github.com/aiplat/core/internal/routing"
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

var _ ports.SemIndex = (*Store)(nil) // compile-time assertion

func semIndexKey(partition string) string { return "SEMIDX#" + partition }

// semIndexCap bounds how many entries one partition keeps (FIFO).
//
// It is a COST dial, not a storage limit. The storage ceiling is DynamoDB's 400KB item:
// at 256 dimensions an int8 vector is 256 bytes, ~344 base64 chars, plus the cache key,
// the two fingerprints and JSON overhead — roughly 470 bytes per entry, so about 850
// entries would fit. The cap is far below that on purpose, because the whole item is read
// on every semantic lookup: tripling the cap triples the bytes read and the RCU per
// request, on the miss path, before the provider is even called. Recall is worth paying
// for; paying for it silently is not.
//
// Override with SEM_INDEX_CAP when a deployment prefers recall over read cost.
const semIndexDefaultCap = 200

// semIndexMaxAttempts bounds the optimistic-locking retry. Three is enough: each retry
// re-reads, and losing three races in a row on one partition means write contention that a
// fourth attempt will not fix either. Giving up is safe — the entry is simply not indexed
// and the next request with the same question indexes it.
const semIndexMaxAttempts = 3

func semIndexCap() int {
	if v, err := strconv.Atoi(os.Getenv("SEM_INDEX_CAP")); err == nil && v > 0 {
		return v
	}
	return semIndexDefaultCap
}

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

// readSemIndex reads a partition's index and the version it was read at.
//
// Absence or error returns an empty list (degradation: with no index the caller simply
// MISSes) and version 0, which the writer treats as "no item yet".
func (s *Store) readSemIndex(ctx context.Context, partition string) ([]SemEntry, int64) {
	if s.table == "" {
		return nil, 0
	}
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table,
		Key:       map[string]ddbtypes.AttributeValue{"cache_key": &ddbtypes.AttributeValueMemberS{Value: semIndexKey(partition)}},
	})
	if err != nil || out.Item == nil {
		return nil, 0
	}
	var version int64
	if vv, ok := out.Item["version"].(*ddbtypes.AttributeValueMemberN); ok {
		version, _ = strconv.ParseInt(vv.Value, 10, 64)
	}
	v, ok := out.Item["idx"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, version
	}
	var entries []SemEntry
	if json.Unmarshal([]byte(v.Value), &entries) != nil {
		return nil, version
	}
	return entries, version
}

// writeSemIndex stores the partition's index CONDITIONALLY on the version it was read at.
//
// This is the fix for a silent lost-write. The previous implementation rewrote the whole
// item with an unconditional PutItem, so two concurrent invocations both read the index,
// both wrote their own entry on top of the version they had read, and the second one
// erased the first. No error, no metric: the index just grew slower than the traffic that
// fed it, which is invisible unless you go looking for it. DynamoDB's conditional write
// turns that race into a detectable conflict the caller can retry.
//
// The TTL stays aligned with the responses: the index expires with them, so a vector whose
// body is already gone is never matched.
func (s *Store) writeSemIndex(ctx context.Context, partition string, entries []SemEntry, version int64, ttlSeconds int) error {
	b, _ := json.Marshal(entries)
	ttl := strconv.FormatInt(time.Now().Unix()+int64(ttlSeconds), 10)

	in := &dynamodb.PutItemInput{TableName: &s.table, Item: map[string]ddbtypes.AttributeValue{
		"cache_key":  &ddbtypes.AttributeValueMemberS{Value: semIndexKey(partition)},
		"idx":        &ddbtypes.AttributeValueMemberS{Value: string(b)},
		"version":    &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version+1, 10)},
		"expires_at": &ddbtypes.AttributeValueMemberN{Value: ttl},
	}}
	if version == 0 {
		// No item yet — or an item written before `version` existed. Either way, only
		// create it if nobody else got there first.
		in.ConditionExpression = aws.String("attribute_not_exists(cache_key) OR attribute_not_exists(version)")
	} else {
		in.ConditionExpression = aws.String("version = :v")
		in.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":v": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(version, 10)},
		}
	}
	_, err := s.ddb.PutItem(ctx, in)
	return err
}

// Search implements ports.SemIndex: it loads the partition's candidates and ranks them
// with the pure domain's similarity.
//
// The ranking deliberately stays in internal/routing rather than being reimplemented
// here: cosine, the int8 quantization and the two exact-match guards (Ctx and Num) are
// property-tested there, and an adapter is the wrong place to keep a second copy of the
// rule that decides whether one customer's answer may be served to another question.
//
// This is the brute-force implementation, and it is honest about that: the scan is linear
// over the partition, which is why semIndexCap exists. A backend with server-side kNN
// implements the same port without shipping the vectors to the caller.
func (s *Store) Search(ctx context.Context, partition string, query []float32, threshold float64, semCtx, semNum string) (string, float64, bool) {
	if s.table == "" || len(query) == 0 {
		return "", 0, false
	}
	entries, _ := s.readSemIndex(ctx, partition)
	if len(entries) == 0 {
		return "", 0, false
	}
	cands := make([]routing.SemCandidate, 0, len(entries))
	for _, e := range entries {
		raw, derr := base64.StdEncoding.DecodeString(e.Q)
		if derr != nil {
			continue
		}
		i8 := make([]int8, len(raw))
		for i, bb := range raw {
			i8[i] = int8(bb)
		}
		cands = append(cands, routing.SemCandidate{
			CacheKey: e.CacheKey,
			Vec:      routing.DequantizeVec(i8, e.Scale),
			Ctx:      e.Ctx,
			Num:      e.Num,
		})
	}
	mt, ok := routing.BestSemanticMatch(query, cands, threshold, semCtx, semNum)
	if !ok {
		return "", 0, false
	}
	return mt.CacheKey, mt.Score, true
}

// Index implements ports.SemIndex: append (cacheKey → vector) with dedup by cacheKey and
// a FIFO cap, under optimistic locking.
//
// An empty semCtx means we do not know how to partition the entry, so it is NOT indexed:
// the reader ignores entries with no context fingerprint, and writing one would create a
// row that can never match anything.
func (s *Store) Index(ctx context.Context, partition, cacheKey string, vec []float32, semCtx, semNum string, ttlSeconds int) {
	if s.table == "" || len(vec) == 0 || semCtx == "" {
		return
	}
	q, scale := routing.QuantizeVec(vec)
	raw := make([]byte, len(q))
	for i, x := range q {
		raw[i] = byte(x)
	}
	entry := SemEntry{
		CacheKey: cacheKey,
		Q:        base64.StdEncoding.EncodeToString(raw),
		Scale:    scale,
		Ctx:      semCtx,
		Num:      semNum,
	}
	cap := semIndexCap()

	for attempt := 0; attempt < semIndexMaxAttempts; attempt++ {
		entries, version := s.readSemIndex(ctx, partition)
		out := make([]SemEntry, 0, cap)
		out = append(out, entry) // newest first; the cap then drops the oldest
		for _, e := range entries {
			if e.CacheKey == cacheKey {
				continue // dedup: this question is being re-indexed
			}
			out = append(out, e)
			if len(out) >= cap {
				break
			}
		}
		if err := s.writeSemIndex(ctx, partition, out, version, ttlSeconds); err == nil {
			return
		}
		// A conditional failure means a concurrent writer won; re-read and merge on top of
		// their version. Any other error is treated the same way — best-effort by contract.
	}
}
