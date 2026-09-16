// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the scope chain — the CORE side (
// task 12, D3, R9.3/9.4, Property 5).
//
// It validates the Core's REAL scopeKeys (the one the gateway uses at runtime)
// against the SAME fixture the Governance validates in
// governance/internal/govcore/scope_contract_test.go. No shared library, no
// cross-domain import: the two implementations are duplicated on purpose (D3) and the
// common fixture is what prevents silent drift.
//
// Note on placement: the Core's chain lives in this adapter (ddbconfig), not in
// internal/routing. That is why the contract test lives here — to exercise the Core's
// REAL code instead of a replica. It is the choice that gives the test teeth.
//
// The Core and Governance chains are now IDENTICAL (ORG#<org>#TEAM#...#APP#...):
// the deployment's single org (DEPLOYMENT_ORG) is a fixed value the Core supplies
// itself, not a per-request parameter — see ddbconfig.go's scopeKeys comment for
// why this must match what Governance writes.
package ddbconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type scopeContractCase struct {
	Name             string   `json:"name"`
	Org              string   `json:"org"`
	Team             string   `json:"team"`
	App              string   `json:"app"`
	CrossDomainEqual bool     `json:"cross_domain_equal"`
	ChainCore        []string `json:"chain_core"`
}

// allowedContractCase drives the allowed_models INTERSECTION rule. Its own local
// struct, decoding only what this side needs — the same pattern as the chain cases,
// and the reason there is no shared Go type across the two domains (D3).
type allowedContractCase struct {
	Name           string                   `json:"name"`
	Levels         []map[string]interface{} `json:"levels"`
	ExpectDeclared bool                     `json:"expect_declared"`
	Expect         []string                 `json:"expect"`
}

type scopeContractFixture struct {
	Cases        []scopeContractCase   `json:"cases"`
	AllowedCases []allowedContractCase `json:"allowed_models_cases"`
}

// From the test's folder (.../core/src/internal/adapters/ddbconfig) to the repository
// root is 6 levels.
const coreFixturePath = "../../../../../../testdata/contracts/config-scope/scope-chain.json"

// TestScopeChainContract_Core validates the Core's own chain (chain_core), which
// never includes an org level: single-org-per-deployment removed that dimension
// from the Core while Governance (still multi-tenant control plane) kept it. Every
// case in the shared fixture is documented as diverging for that structural
// reason, not as a data mismatch — see the fixture's top-level "note".
func TestScopeChainContract_Core(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(coreFixturePath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var f scopeContractFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture with no cases")
	}
	for _, c := range f.Cases {
		if got := scopeKeys(c.Org, c.Team, c.App); !reflect.DeepEqual(got, c.ChainCore) {
			t.Errorf("[%s] scopeKeys(%q,%q,%q)=%v, the contract wants %v", c.Name, c.Org, c.Team, c.App, got, c.ChainCore)
		}
	}
}

// CONTRACT test for the allowed_models INTERSECTION rule — the CORE side.
//
// It folds the fixture's levels with foldAllowed, which is the exact code the merge
// walk in Effective runs (Effective itself needs a DynamoDB client, so the fold is
// factored out precisely so this test can reach it). Governance verifies the SAME
// cases against govcore.IntersectAllowed with no shared code.
//
// What this pins, and why it matters more than the arithmetic: the difference between
// "not declared" (key absent → no restriction) and "declared empty" (→ deny
// everything). The predicate used to read len==0 as "allow everything", so
// intersecting a team's [a] with an app's [b] would have granted the whole catalog.
func TestAllowedModelsIntersectionContract_Core(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(coreFixturePath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var f scopeContractFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	if len(f.AllowedCases) == 0 {
		t.Fatal("fixture has no allowed_models_cases — the contract would pass over nothing")
	}
	for _, c := range f.AllowedCases {
		acc, declared := []interface{}(nil), false
		for _, level := range c.Levels {
			acc, declared = foldAllowed(acc, declared, level)
		}
		if declared != c.ExpectDeclared {
			t.Errorf("[%s] declared=%v, the contract wants %v", c.Name, declared, c.ExpectDeclared)
			continue
		}
		if !declared {
			continue
		}
		got := make([]string, 0, len(acc))
		for _, v := range acc {
			s, ok := v.(string)
			if !ok {
				t.Errorf("[%s] a non-string leaked into the effective list: %#v", c.Name, v)
				continue
			}
			got = append(got, s)
		}
		want := c.Expect
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("[%s] effective=%v, the contract wants %v", c.Name, got, want)
		}
	}
}

// The intersection must never hand back a slice that aliases either input. Effective
// mutates `base` in place and then caches it BY REFERENCE for 15s, so an in-place
// filter would corrupt a map the caller still holds and the cache is about to serve.
func TestIntersectAllowed_DoesNotAliasItsInputs(t *testing.T) {
	parent := []interface{}{"a", "b", "c"}
	child := []interface{}{"a", "b"}
	got, declared := intersectAllowed(parent, true, child, true)
	if !declared || len(got) != 2 {
		t.Fatalf("got %v declared=%v", got, declared)
	}
	got[0] = "MUTATED"
	if parent[0] != "a" || child[0] != "a" {
		t.Fatalf("writing to the result mutated an input: parent=%v child=%v", parent, child)
	}

	// The pass-through branches copy too — they are the ones most likely to be
	// written as `return child`.
	fromChild, _ := intersectAllowed(nil, false, child, true)
	fromChild[0] = "MUTATED"
	if child[0] != "a" {
		t.Fatalf("the parent-undeclared branch aliased the child: %v", child)
	}
	fromParent, _ := intersectAllowed(parent, true, nil, false)
	fromParent[0] = "MUTATED"
	if parent[0] != "a" {
		t.Fatalf("the child-undeclared branch aliased the parent: %v", parent)
	}
}

// declaredList is where the nil/empty distinction is read off the stored document.
func TestDeclaredList(t *testing.T) {
	if _, ok := declaredList(nil); ok {
		t.Error("an absent key must not count as declared")
	}
	if _, ok := declaredList("not a list"); ok {
		t.Error("a non-list value must not count as declared")
	}
	got, ok := declaredList([]interface{}{})
	if !ok || got == nil || len(got) != 0 {
		t.Errorf("a declared EMPTY list must be declared and empty, got %v ok=%v", got, ok)
	}
}
