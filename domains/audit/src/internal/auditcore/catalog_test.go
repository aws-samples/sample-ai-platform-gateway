// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the vocabulary of auditable actions.
//
// It validates auditcore.Catalog() against the shared fixture in
// testdata/contracts/audit-trail/action-catalog.json. The SAME fixture is validated,
// with no common library, by the domains that EMIT events (governance, core,
// backoffice). That is how the vocabulary stays coherent across domains without one
// importing the other — a cross-domain import is forbidden and verified by
// boundary_test.go.
package auditcore

import (
	"encoding/json"
	"os"
	"testing"
)

// From this test's folder (.../audit/src/internal/auditcore) up to the repository root
// is 5 levels, then the fixture path.
const fixturePath = "../../../../../testdata/contracts/audit-trail/action-catalog.json"

type catalogFixture struct {
	ContractVersion string            `json:"contract_version"`
	Categories      []string          `json:"categories"`
	Actions         map[string]string `json:"actions"`
}

func loadFixture(t *testing.T) catalogFixture {
	t.Helper()
	b, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var f catalogFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	if len(f.Actions) == 0 {
		t.Fatal("fixture has no actions — an empty contract validates nothing")
	}
	return f
}

func TestCatalogo_BateComOFixtureCompartilhado(t *testing.T) {
	f := loadFixture(t)
	got := Catalog()

	// Every action in the fixture exists in the code, with the SAME category.
	for action, wantCat := range f.Actions {
		cat, ok := CategoryOf(action)
		if !ok {
			t.Errorf("action %q is in the fixture and missing from the code", action)
			continue
		}
		if cat != wantCat {
			t.Errorf("action %q: category = %q, the fixture says %q", action, cat, wantCat)
		}
	}
	// And the reverse: no extra action in the code that is not in the contract. Without
	// this half, the code could gain vocabulary the emitters do not know about.
	for action := range got {
		if _, ok := f.Actions[action]; !ok {
			t.Errorf("action %q is in the code and missing from the fixture", action)
		}
	}
}

func TestCatalogo_CategoriasBatemComOFixture(t *testing.T) {
	f := loadFixture(t)
	if len(f.Categories) != len(Categories()) {
		t.Fatalf("categories: the code has %d, the fixture has %d", len(Categories()), len(f.Categories))
	}
	for _, c := range f.Categories {
		if !ValidCategory(c) {
			t.Errorf("category %q from the fixture is not valid in the code", c)
		}
	}
}

// An unknown action must not be treated as a fatal error: the writer needs to persist
// the record anyway (Req 2.8). This test pins the contract that CategoryOf merely
// SIGNALS that it does not recognize the action.
func TestCategoryOf_AcaoDesconhecida(t *testing.T) {
	cat, ok := CategoryOf("acao_que_nao_existe")
	if ok {
		t.Error("a nonexistent action should not be recognized")
	}
	if cat != "" {
		t.Errorf("category of an unknown action = %q, expected empty", cat)
	}
}

// The seven member actions have FROZEN string values: they already exist in
// govcore.Audit* and may already have been recorded. An audit record is immutable —
// renaming would leave history with two names for the same thing.
func TestAcoesDeMembro_ValoresCongelados(t *testing.T) {
	esperado := map[string]string{
		ActionMemberInvite:  "member_invite",
		ActionMemberUpdate:  "member_update",
		ActionMemberRemove:  "member_remove",
		ActionMemberEnable:  "member_enable",
		ActionMemberDisable: "member_disable",
		ActionPasswordReset: "password_reset",
		ActionInviteResend:  "invite_resend",
	}
	for got, want := range esperado {
		if got != want {
			t.Errorf("constant changed value: %q, expected %q", got, want)
		}
	}
}

// Catalog returns a copy: mutating the result must not alter the vocabulary.
func TestCatalog_DevolveCopia(t *testing.T) {
	c := Catalog()
	c[ActionConfigUpdate] = "categoria_falsa"
	if cat, _ := CategoryOf(ActionConfigUpdate); cat != CatConfig {
		t.Errorf("mutating the copy changed the catalog: %q", cat)
	}
}
