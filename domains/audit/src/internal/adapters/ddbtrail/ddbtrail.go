// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ddbtrail is the DynamoDB adapter for the audit trail.
//
// It concentrates three infrastructure decisions the pure domain does not know about:
//  1. the INDEX CHOICE according to the filter (category LSI, actor LSI, or the base
//     key);
//  2. the CONDITIONAL write that provides idempotency;
//  3. serializing the diff as a JSON string.
package ddbtrail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/aiplat/audit/internal/auditcore"
	"github.com/aiplat/audit/internal/ports"
)

var _ ports.TrailStore = (*Adapter)(nil)

// Names of the local indexes. They match the Terraform; diverging here returns an
// empty query with no error, which is the hardest failure to notice.
const (
	IndexByCategory = "by_category"
	IndexByActor    = "by_actor"
)

type Adapter struct {
	ddb   *dynamodb.Client
	table string
}

func New(ddb *dynamodb.Client, table string) *Adapter {
	return &Adapter{ddb: ddb, table: table}
}

func s(v string) *ddbtypes.AttributeValueMemberS { return &ddbtypes.AttributeValueMemberS{Value: v} }
func n(v int64) *ddbtypes.AttributeValueMemberN {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
func str(av ddbtypes.AttributeValue) string {
	if v, ok := av.(*ddbtypes.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}
func num(av ddbtypes.AttributeValue) int {
	if v, ok := av.(*ddbtypes.AttributeValueMemberN); ok {
		i, _ := strconv.Atoi(v.Value)
		return i
	}
	return 0
}
func boolOf(av ddbtypes.AttributeValue) bool {
	if v, ok := av.(*ddbtypes.AttributeValueMemberBOOL); ok {
		return v.Value
	}
	return false
}
func ptr(v string) *string { return &v }

// Append writes the record. The write is conditioned on `attribute_not_exists(sk)`,
// which gives idempotency for free: SQS delivery is at-least-once, so reprocessing
// the same message is normal, not exceptional.
//
// ConditionalCheckFailed is treated as SUCCESS — the record is already there, which
// is exactly the desired state. Returning an error would send the message back to the
// queue in an endless loop and end up in the DLQ, signalling a problem that does not
// exist.
func (a *Adapter) Append(ctx context.Context, ev auditcore.Event, expiresAt int64) error {
	sk, catSK, actorSK := auditcore.SortKeys(ev.TS, ev.EventID, ev.Category, ev.Actor.Email)

	// The diff goes as a JSON string, not as a native list: `before`/`after` are
	// heterogeneous (number, bool, string, object) and mapping that to nested
	// AttributeValue would only add fragile conversion. The config already uses the
	// same approach in gov-config.
	changesJSON := "[]"
	if len(ev.Changes) > 0 {
		if b, err := json.Marshal(ev.Changes); err == nil {
			changesJSON = string(b)
		}
	}

	item := map[string]ddbtypes.AttributeValue{
		"pk":       s(auditcore.PartitionKey(ev.Org)),
		"sk":       s(sk),
		"cat_sk":   s(catSK),
		"actor_sk": s(actorSK),

		"event_id": s(ev.EventID),
		"action":   s(ev.Action),
		"category": s(ev.Category),

		"actor_email": s(ev.Actor.Email),
		"actor_role":  s(ev.Actor.Role),
		"actor_type":  s(ev.Actor.Type),

		"changes":      s(changesJSON),
		"change_count": n(int64(ev.ChangeCount)),

		"ts":         s(ev.TS),
		"expires_at": n(expiresAt),
	}
	// Optional fields only go in when they have a value — smaller item, and absence
	// stays distinguishable from empty on read.
	for k, v := range map[string]string{
		"actor_sub":  ev.Actor.Sub,
		"scope":      ev.Scope,
		"target":     ev.Target,
		"detail":     ev.Detail,
		"source_ip":  ev.SourceIP,
		"user_agent": ev.UserAgent,
	} {
		if v != "" {
			item[k] = s(v)
		}
	}
	if ev.Truncated {
		item["truncated"] = &ddbtypes.AttributeValueMemberBOOL{Value: true}
	}

	_, err := a.ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &a.table,
		Item:                item,
		ConditionExpression: ptr("attribute_not_exists(sk)"),
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return nil // already recorded
		}
		return err
	}
	return nil
}

// Query builds the request, choosing the index from the filter. The order of
// preference is not arbitrary: category comes first because it is the DOMINANT access
// pattern (each Console sub-tab is a category). Resolving category via
// FilterExpression would read the entire time window only to discard most of it.
func (a *Adapter) Query(ctx context.Context, q ports.TrailQuery) ([]auditcore.Event, string, error) {
	from, to := q.FromTS, q.ToTS
	if from == "" {
		from = "0000"
	}
	if to == "" {
		to = "9999"
	}

	in := &dynamodb.QueryInput{
		TableName:        &a.table,
		ScanIndexForward: boolPtr(false), // descending: most recent first
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":p": s(auditcore.PartitionKey(q.Org)),
		},
	}

	switch {
	case q.Category != "":
		pre := auditcore.CategoryPrefix(q.Category)
		in.IndexName = ptr(IndexByCategory)
		in.KeyConditionExpression = ptr("pk = :p AND cat_sk BETWEEN :a AND :b")
		in.ExpressionAttributeValues[":a"] = s(pre + from)
		in.ExpressionAttributeValues[":b"] = s(pre + to + "~")
	case q.Actor != "":
		pre := auditcore.ActorPrefix(q.Actor)
		in.IndexName = ptr(IndexByActor)
		in.KeyConditionExpression = ptr("pk = :p AND actor_sk BETWEEN :a AND :b")
		in.ExpressionAttributeValues[":a"] = s(pre + from)
		in.ExpressionAttributeValues[":b"] = s(pre + to + "~")
	default:
		in.KeyConditionExpression = ptr("pk = :p AND sk BETWEEN :a AND :b")
		in.ExpressionAttributeValues[":a"] = s("TS#" + from)
		in.ExpressionAttributeValues[":b"] = s("TS#" + to + "~")
	}

	// action and target are refinements INSIDE an already narrow slice — a post-Query
	// filter is the right choice here; spending an LSI on them would optimize the rare
	// case.
	var filters []string
	names := map[string]string{}
	if q.Action != "" {
		filters = append(filters, "#act = :action")
		names["#act"] = "action" // "action" is a reserved word in DynamoDB
		in.ExpressionAttributeValues[":action"] = s(q.Action)
	}
	if q.Target != "" {
		filters = append(filters, "target = :target")
		in.ExpressionAttributeValues[":target"] = s(q.Target)
	}
	if len(filters) > 0 {
		in.FilterExpression = ptr(strings.Join(filters, " AND "))
	}
	if len(names) > 0 {
		in.ExpressionAttributeNames = names
	}
	if q.Limit > 0 {
		in.Limit = int32Ptr(int32(q.Limit))
	}
	if q.Token != "" {
		lek, err := decodeToken(q.Token)
		if err != nil {
			return nil, "", err
		}
		in.ExclusiveStartKey = lek
	}

	out, err := a.ddb.Query(ctx, in)
	if err != nil {
		return nil, "", err
	}
	evs := make([]auditcore.Event, 0, len(out.Items))
	for _, it := range out.Items {
		evs = append(evs, itemToEvent(it))
	}
	next := ""
	if out.LastEvaluatedKey != nil {
		if t, err := encodeToken(out.LastEvaluatedKey); err == nil {
			next = t
		}
	}
	return evs, next, nil
}

func itemToEvent(it map[string]ddbtypes.AttributeValue) auditcore.Event {
	ev := auditcore.Event{
		ContractVersion: auditcore.ContractVersion,
		EventID:         str(it["event_id"]),
		Org:             strings.TrimPrefix(str(it["pk"]), "AUDIT#"),
		Action:          str(it["action"]),
		Category:        str(it["category"]),
		Scope:           str(it["scope"]),
		Target:          str(it["target"]),
		Detail:          str(it["detail"]),
		ChangeCount:     num(it["change_count"]),
		Truncated:       boolOf(it["truncated"]),
		SourceIP:        str(it["source_ip"]),
		UserAgent:       str(it["user_agent"]),
		TS:              str(it["ts"]),
		Actor: auditcore.Actor{
			Email: str(it["actor_email"]),
			Sub:   str(it["actor_sub"]),
			Role:  str(it["actor_role"]),
			Type:  str(it["actor_type"]),
		},
	}
	if raw := str(it["changes"]); raw != "" {
		var chs []auditcore.Change
		if json.Unmarshal([]byte(raw), &chs) == nil {
			ev.Changes = chs
		}
	}
	return ev
}

// The pagination token is opaque to the client (base64 of the LastEvaluatedKey).
// Opaque on purpose: exposing the internal key would become an accidental contract and
// would freeze schema evolution.
func encodeToken(lek map[string]ddbtypes.AttributeValue) (string, error) {
	plain := map[string]string{}
	for k, v := range lek {
		plain[k] = str(v)
	}
	b, err := json.Marshal(plain)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeToken(tok string) (map[string]ddbtypes.AttributeValue, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, err
	}
	var plain map[string]string
	if err := json.Unmarshal(b, &plain); err != nil {
		return nil, err
	}
	out := map[string]ddbtypes.AttributeValue{}
	for k, v := range plain {
		out[k] = s(v)
	}
	return out, nil
}

func boolPtr(v bool) *bool    { return &v }
func int32Ptr(v int32) *int32 { return &v }
