// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbkeys is the outbound adapter that resolves the API key (hash) into
// org/team/app in the Core's API keys table. It implements ports.KeyStore.
//
// Feature: hexagonal-refactor, task 4.7. Code MOVED from the DynamoDB part of
// authResolve (cmd/router/main.go) without rewriting the logic. Extracting the key
// from the header (Bearer / x-aiplat-key) stays in the handler — that is protocol
// shell, not table mechanics. It preserves: consistent read (a freshly issued key
// works immediately), a revoked key stays invalid, and compatibility with old keys
// (legacy `tenant`/`app_tag` fields) and the default team.
package ddbkeys

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

// Store is the production adapter over the API keys table.
type Store struct {
	ddb   *dynamodb.Client
	table string
}

// New builds the adapter with the client and the table name (may be "").
func New(ddb *dynamodb.Client, table string) *Store {
	return &Store{ddb: ddb, table: table}
}

var _ ports.KeyStore = (*Store)(nil) // compile-time assertion

// Enabled tells whether the keys table is configured.
func (s *Store) Enabled() bool { return s.table != "" }

// Resolve validates the key (hash) and returns org/team/app.
func (s *Store) Resolve(ctx context.Context, key string) (ports.KeyIdentity, bool, error) {
	if key == "" || s.table == "" {
		return ports.KeyIdentity{}, false, nil
	}
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])
	out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.table,
		Key:            map[string]ddbtypes.AttributeValue{"api_key_hash": &ddbtypes.AttributeValueMemberS{Value: hash}},
		ConsistentRead: aws.Bool(true), // a freshly issued key works immediately
	})
	if err != nil {
		return ports.KeyIdentity{}, false, err // PLATFORM: auth backend unavailable
	}
	if out.Item == nil {
		return ports.KeyIdentity{}, false, nil // key does not exist → unauthorized
	}
	str := func(k string) string {
		if v, ok := out.Item[k].(*ddbtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	// A logically revoked key stays invalid.
	if st := str("status"); st != "" && st != "active" {
		return ports.KeyIdentity{}, false, nil
	}
	// Single-org model: org is deployment-level, not key-level.
	// Compatibility: old keys may still have tenant/org_id fields (ignored).
	id := ports.KeyIdentity{Team: str("team_id"), App: str("app")}
	if id.App == "" {
		id.App = str("app_tag")
	}
	if id.Team == "" {
		id.Team = "default"
	}
	// Org field removed - always empty in single-org deployments
	return id, true, nil
}
