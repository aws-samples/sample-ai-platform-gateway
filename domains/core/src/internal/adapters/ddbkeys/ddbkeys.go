// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbkeys is the outbound adapter that resolves the API key (hash) into
// org/team/app in the Core's API keys table. It implements ports.KeyStore.
//
// authResolve (cmd/router/main.go) without rewriting the logic. Extracting the key
// from the header (Bearer / x-aiplat-key) stays in the handler — that is protocol
// shell, not table mechanics. It preserves: consistent read (a freshly issued key
// works immediately), a revoked key stays invalid, and compatibility with old keys
// (legacy `tenant`/`app_tag` fields) and the default team.
package ddbkeys

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/core/internal/ports"
)

// ErrNoKeysTable is returned when the API keys table is not configured. CORE-SEC-008: a
// missing table is a DEPLOYMENT fault, not a bad credential — surfacing it as an error
// makes the handler answer 503 (and count it as OUR failure in the SLI) instead of 401,
// which mis-attributed a misdeploy against the client.
var ErrNoKeysTable = errors.New("api keys table is not configured")

// Store is the production adapter over the API keys table.
type Store struct {
	ddb    *dynamodb.Client
	table  string
	pepper []byte
}

// New builds the adapter with the client and the table name (may be ""). The optional
// KEY_HASH_PEPPER (from Secrets Manager via the runtime env) turns the key hash into an
// HMAC; when unset the adapter uses the legacy unsalted SHA-256, so behaviour is
// unchanged until a pepper is provisioned.
func New(ddb *dynamodb.Client, table string) *Store {
	return &Store{ddb: ddb, table: table, pepper: []byte(os.Getenv("KEY_HASH_PEPPER"))}
}

var _ ports.KeyStore = (*Store)(nil) // compile-time assertion

// Enabled tells whether the keys table is configured.
func (s *Store) Enabled() bool { return s.table != "" }

// candidateHashes lists the stored-hash forms to look up, in priority order (CORE-SEC-008):
// the peppered HMAC first when a pepper is configured, then the legacy unsalted SHA-256 so
// keys issued before the pepper keep resolving during migration.
func (s *Store) candidateHashes(key string) []string {
	var hashes []string
	if len(s.pepper) > 0 {
		m := hmac.New(sha256.New, s.pepper)
		m.Write([]byte(key))
		hashes = append(hashes, hex.EncodeToString(m.Sum(nil)))
	}
	sum := sha256.Sum256([]byte(key))
	return append(hashes, hex.EncodeToString(sum[:]))
}

// Resolve validates the key (hash) and returns org/team/app.
func (s *Store) Resolve(ctx context.Context, key string) (ports.KeyIdentity, bool, error) {
	if s.table == "" {
		return ports.KeyIdentity{}, false, ErrNoKeysTable // PLATFORM: misconfigured, not a bad key
	}
	if key == "" {
		return ports.KeyIdentity{}, false, nil
	}
	for _, hash := range s.candidateHashes(key) {
		out, err := s.ddb.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:      &s.table,
			Key:            map[string]ddbtypes.AttributeValue{"api_key_hash": &ddbtypes.AttributeValueMemberS{Value: hash}},
			ConsistentRead: aws.Bool(true), // a freshly issued key works immediately
		})
		if err != nil {
			return ports.KeyIdentity{}, false, err // PLATFORM: auth backend unavailable
		}
		if out.Item == nil {
			continue // not under this hash form — try the next candidate
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
		id := ports.KeyIdentity{Team: str("team_id"), App: str("app"), Apps: strs(out.Item, "apps")}
		if id.App == "" {
			id.App = str("app_tag")
		}
		if id.Team == "" {
			id.Team = "default"
		}
		// Org field removed - always empty in single-org deployments
		return id, true, nil
	}
	return ports.KeyIdentity{}, false, nil // key does not exist → unauthorized
}

// strs reads a multi-app allowlist, accepting either a DynamoDB string set or a list.
//
// Both shapes are read because the two are indistinguishable to a human editing an item
// in the console and easy to mix up in a script: keyadmin writes a string set, and a key
// patched by hand with a list would otherwise resolve to "no extra apps allowed" — a
// silent 403 on every request naming a project. Absent means the key is single-app,
// which is every key issued before this existed.
func strs(item map[string]ddbtypes.AttributeValue, k string) []string {
	switch v := item[k].(type) {
	case *ddbtypes.AttributeValueMemberSS:
		return v.Value
	case *ddbtypes.AttributeValueMemberL:
		out := make([]string, 0, len(v.Value))
		for _, e := range v.Value {
			if s, ok := e.(*ddbtypes.AttributeValueMemberS); ok && s.Value != "" {
				out = append(out, s.Value)
			}
		}
		return out
	}
	return nil
}
