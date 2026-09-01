// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// ORCHESTRATION tests for audit-writer: they verify that the shell wires parsing →
// pure domain → port in the right order, using in-memory doubles. They do not test
// DynamoDB (that is the adapter's job) nor the arithmetic (that is the pure domain's).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aiplat/audit/internal/adapters/inmem"
	"github.com/aiplat/audit/internal/auditcore"
	"github.com/aiplat/audit/internal/ports"
)

func setup(t *testing.T) *inmem.Trail {
	t.Helper()
	tr := inmem.NewTrail()
	trail = tr
	return tr
}

func q(org string) ports.TrailQuery { return ports.TrailQuery{Org: org} }

func body(ev auditcore.Event) string {
	b, _ := json.Marshal(map[string]any{
		"detail-type": "aiplat.audit", "source": "aiplat.governance", "detail": ev,
	})
	return string(b)
}

func baseEvent() auditcore.Event {
	return auditcore.Event{
		ContractVersion: auditcore.ContractVersion,
		EventID:         "evt-1",
		Org:             "org_x",
		Actor:           auditcore.NewActor("admin@corp.com", "sub-1", auditcore.RoleAdmin),
		Action:          auditcore.ActionConfigUpdate,
		Scope:           "ORG#org_x",
		TS:              "2026-08-13T22:39:40Z",
		Changes: []auditcore.Change{
			{Path: "budget.limit_usd", Before: float64(100), After: float64(50)},
		},
		ChangeCount: 1,
	}
}

func TestWriter_PersisteEDerivaCategoria(t *testing.T) {
	tr := setup(t)

	if err := one(context.Background(), body(baseEvent())); err != nil {
		t.Fatalf("one: %v", err)
	}
	if tr.Appends != 1 {
		t.Fatalf("writes = %d, expected 1", tr.Appends)
	}
	evs, _, _ := tr.Query(context.Background(), q("org_x"))
	if len(evs) != 1 {
		t.Fatalf("records = %d", len(evs))
	}
	if evs[0].Category != auditcore.CatConfig {
		t.Errorf("category = %q, expected %q", evs[0].Category, auditcore.CatConfig)
	}
}

// Property 4: reprocessing the same message does not create a second record. SQS
// delivery is at-least-once, so this is the normal path, not the exceptional one.
func TestWriter_Idempotente(t *testing.T) {
	tr := setup(t)
	msg := body(baseEvent())

	for i := 0; i < 3; i++ {
		if err := one(context.Background(), msg); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if tr.Appends != 1 {
		t.Errorf("effective writes = %d, expected 1 (idempotency)", tr.Appends)
	}
}

// Two DIFFERENT events at the same instant must both land — that is what the
// event_id in the key protects. Without it, idempotency would eat a legitimate record.
func TestWriter_EventosDistintosNoMesmoInstante(t *testing.T) {
	tr := setup(t)

	a := baseEvent()
	b := baseEvent()
	b.EventID = "evt-2"
	b.Action = auditcore.ActionModelAdd

	for _, ev := range []auditcore.Event{a, b} {
		if err := one(context.Background(), body(ev)); err != nil {
			t.Fatalf("one: %v", err)
		}
	}
	if tr.Appends != 2 {
		t.Errorf("writes = %d, expected 2", tr.Appends)
	}
}

// Single-client deployment: retention is a fixed constant, not a plan lookup.
func TestWriter_TTLFixo(t *testing.T) {
	tr := setup(t)
	ev := baseEvent()
	if err := one(context.Background(), body(ev)); err != nil {
		t.Fatalf("one: %v", err)
	}
	ev.Category = auditcore.CatConfig // the writer fills this in before writing
	base, _ := time.Parse(time.RFC3339, ev.TS)
	want := base.AddDate(0, 0, auditcore.RetentionDays).Unix()
	if got := tr.TTLOf(ev); got != want {
		t.Errorf("expires_at = %d, expected %d", got, want)
	}
}

// Property 2 at ingestion: a buggy emitter that lets a secret through must not be
// able to persist it. An append-only record cannot be fixed later.
func TestWriter_RedigeNaIngestao(t *testing.T) {
	tr := setup(t)

	ev := baseEvent()
	ev.Changes = []auditcore.Change{
		{Path: "provider.api_key", Before: "sk-vazou-123", After: "sk-vazou-456"},
	}
	if err := one(context.Background(), body(ev)); err != nil {
		t.Fatalf("one: %v", err)
	}
	evs, _, _ := tr.Query(context.Background(), q("org_x"))
	b, _ := json.Marshal(evs)
	if strings.Contains(string(b), "sk-vazou") {
		t.Errorf("secret persisted: %s", b)
	}
	if !evs[0].Changes[0].Redacted {
		t.Error("sensitive change should be marked as redacted")
	}
}

// Req 2.8: an unknown action is PERSISTED, not dropped.
func TestWriter_AcaoDesconhecidaNaoEDescartada(t *testing.T) {
	tr := setup(t)

	ev := baseEvent()
	ev.Action = "acao_de_emissor_mais_novo"
	if err := one(context.Background(), body(ev)); err != nil {
		t.Fatalf("one: %v", err)
	}
	if tr.Appends != 1 {
		t.Fatalf("unknown action should be persisted, writes = %d", tr.Appends)
	}
	evs, _, _ := tr.Query(context.Background(), q("org_x"))
	if evs[0].Action != "acao_de_emissor_mais_novo" {
		t.Errorf("action = %q, should preserve the name received", evs[0].Action)
	}
	if evs[0].Category != "other" {
		t.Errorf("category = %q, expected 'other'", evs[0].Category)
	}
}

// A store failure must return an error, so the message goes back to the queue and
// ends up in the DLQ. Absorbing it here would silently lose audit data (violates
// Property 7).
func TestWriter_FalhaDoStoreVoltaParaAFila(t *testing.T) {
	tr := setup(t)
	tr.AppendErr = errors.New("dynamodb unavailable")

	if err := one(context.Background(), body(baseEvent())); err == nil {
		t.Error("a store failure should propagate so SQS redelivers")
	}
}

// An unreadable message is absorbed (with a log): a retry does not help and it would
// block the queue forever.
func TestWriter_MensagemIlegivelNaoBloqueiaAFila(t *testing.T) {
	tr := setup(t)
	if err := one(context.Background(), "{this is not json"); err != nil {
		t.Errorf("an unreadable message should not propagate an error: %v", err)
	}
	if tr.Appends != 0 {
		t.Error("nothing should be written")
	}
}

func TestWriter_EventoIncompletoEAbsorvido(t *testing.T) {
	tr := setup(t)
	ev := baseEvent()
	ev.Org = "" // without an org there is no possible partition
	if err := one(context.Background(), body(ev)); err != nil {
		t.Errorf("an incomplete event should not propagate an error: %v", err)
	}
	if tr.Appends != 0 {
		t.Error("should not write an event without an org")
	}
}
