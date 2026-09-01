// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Audit event emission for keyadmin (Core domain).
//
// Local to the Core on purpose: a domain does not import another domain. Vocabulary
// coherence with the Audit domain is guaranteed by a shared FIXTURE
// (testdata/contracts/audit-trail/action-catalog.json), validated by a test.
//
// Two rules that cannot be relaxed:
//  1. emitting NEVER fails the key issue/revoke;
//  2. the structured log comes BEFORE publishing — if PutEvents fails, the line is
//     already in CloudWatch and the trail can be rebuilt with Logs Insights.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/eventbridge"
	ebtypes "github.com/aws/aws-sdk-go-v2/service/eventbridge/types"
)

// Auditable actions of this component. Category "keys" in the catalog.
const (
	audKeyIssue  = "key_issue"
	audKeyRevoke = "key_revoke"
)

// An empty auditBus TURNS OFF emission without breaking anything — it lets this
// domain ship before the Audit domain exists.
var (
	auditBus = os.Getenv("AUDIT_BUS")
	eb       *eventbridge.Client
	auditSeq int64
)

type auditActor struct {
	Email string `json:"email"`
	Sub   string `json:"sub,omitempty"`
	Role  string `json:"role"`
	Type  string `json:"type"`
}

type auditEvent struct {
	ContractVersion string     `json:"contract_version"`
	EventID         string     `json:"event_id"`
	Org             string     `json:"org"`
	Actor           auditActor `json:"actor"`
	Action          string     `json:"action"`
	Category        string     `json:"category"`
	Scope           string     `json:"scope,omitempty"`
	Target          string     `json:"target,omitempty"`
	Detail          string     `json:"detail,omitempty"`
	ChangeCount     int        `json:"change_count"`
	SourceIP        string     `json:"source_ip,omitempty"`
	UserAgent       string     `json:"user_agent,omitempty"`
	TS              string     `json:"ts"`
}

type auditCtx struct {
	actor     auditActor
	sourceIP  string
	userAgent string
}

func newAuditActor(email, sub, role string) auditActor {
	typ := "customer"
	if role == "platform_admin" {
		typ = "platform_operator"
	}
	return auditActor{Email: lower(email), Sub: sub, Role: role, Type: typ}
}

// emitAudit publishes the audit event. It NEVER propagates an error.
//
// target is the key PREFIX, never the key: the trail identifies which key was
// touched without becoming a credential store.
func emitAudit(ctx context.Context, ac auditCtx, action, org, team, app, keyPrefix string) {
	if org == "" || action == "" {
		return
	}
	auditSeq++
	ev := auditEvent{
		ContractVersion: "1",
		EventID:         strconv.FormatInt(time.Now().UTC().UnixNano(), 36) + "-" + strconv.FormatInt(auditSeq, 36),
		Org:             org,
		Actor:           ac.actor,
		Action:          action,
		Category:        "keys",
		Target:          keyPrefix,
		Detail:          "time=" + team + " app=" + app,
		SourceIP:        ac.sourceIP,
		UserAgent:       ac.userAgent,
		TS:              time.Now().UTC().Format(time.RFC3339),
	}

	// 1) Raw trail in our own log group, BEFORE publishing.
	if b, err := json.Marshal(map[string]interface{}{
		"event": "audit", "action": ev.Action, "org": ev.Org, "actor": ev.Actor.Email,
		"role": ev.Actor.Role, "target": ev.Target, "detail": ev.Detail, "event_id": ev.EventID,
	}); err == nil {
		log.Println(string(b))
	}

	// 2) Best-effort publication.
	if auditBus == "" || eb == nil {
		return
	}
	detail, err := json.Marshal(ev)
	if err != nil {
		return
	}
	src, dt, bus := "aiplat.core", "aiplat.audit", auditBus
	if _, err := eb.PutEvents(ctx, &eventbridge.PutEventsInput{
		Entries: []ebtypes.PutEventsRequestEntry{{
			EventBusName: &bus, Source: &src, DetailType: &dt, Detail: strPtr(string(detail)),
		}},
	}); err != nil {
		log.Printf(`{"event":"audit_emit_failed","action":%q,"org":%q,"error":%q}`, ev.Action, ev.Org, err.Error())
	}
}

func strPtr(v string) *string { return &v }

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
