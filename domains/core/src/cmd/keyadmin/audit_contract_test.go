// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the audit vocabulary emitted by the Core.
//
// The Core does not import the Audit domain (cross-domain imports are forbidden and
// verified by boundary_test), so vocabulary coherence is guaranteed against the
// shared fixture. Without this test, renaming an action on one side would leave the
// trail with records the UI cannot label — and audit records are immutable, so there
// is no fixing it afterwards.
package main

import (
	"encoding/json"
	"os"
	"testing"
)

// From this test's folder (.../core/src/cmd/keyadmin) to the repository root is
// 5 levels.
const auditFixturePath = "../../../../../testdata/contracts/audit-trail/action-catalog.json"

func TestAuditVocabulario_BateComOFixture(t *testing.T) {
	b, err := os.ReadFile(auditFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var f struct {
		Actions map[string]string `json:"actions"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	// The actions THIS component emits, with the category it declares.
	emitidas := map[string]string{
		audKeyIssue:  "keys",
		audKeyRevoke: "keys",
	}
	for action, cat := range emitidas {
		got, ok := f.Actions[action]
		if !ok {
			t.Errorf("action %q is emitted by keyadmin and is not in the catalog", action)
			continue
		}
		if got != cat {
			t.Errorf("action %q: keyadmin declares category %q, the catalog says %q", action, cat, got)
		}
	}
}

// The trail identifies the key only by its PREFIX. This test pins the guarantee that
// no field of the event carries the whole key — auditing must not become a
// credential store.
func TestAuditEvento_NaoCarregaChaveInteira(t *testing.T) {
	const chave = "sk-aiplat-0e712c14f014eabebf8cc0b0cd413474f9147227b0fd2a98"
	const prefixo = "sk-aiplat-0e71…"

	ev := auditEvent{
		ContractVersion: "1", EventID: "e1", Org: "org_x",
		Actor:  newAuditActor("admin@corp.com", "s", "owner"),
		Action: audKeyIssue, Category: "keys",
		Target: prefixo, Detail: "time=plataforma app=api",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(b); contains(got, chave) {
		t.Errorf("the whole key leaked into the event: %s", got)
	}
}

func TestNewAuditActor_NormalizaEClassifica(t *testing.T) {
	a := newAuditActor("Admin@Corp.COM", "s", "owner")
	if a.Email != "admin@corp.com" {
		t.Errorf("email not normalized: %q", a.Email)
	}
	if a.Type != "customer" {
		t.Errorf("type = %q, expected customer", a.Type)
	}
	if op := newAuditActor("ops@aiplat.local", "s", "platform_admin"); op.Type != "platform_operator" {
		t.Errorf("platform_admin should be an operator, got %q", op.Type)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
