// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the scope chain (D3, R9.3/9.4,
// Property 5).
//
// Validates GOVERNANCE's govcore.ScopeKeys/ScopeKey against the shared fixture at
// testdata/contracts/config-scope/scope-chain.json. The SAME fixture is
// validated, with no common library, on the Core side
// (core/internal/adapters/ddbconfig/scope_contract_test.go). That is how the
// legitimate duplication of the rule (D3) is protected from drift.
//
// Known divergence ("org alone"): the fixture marks the case with
// cross_domain_equal=false and each side validates its OWN chain. See the note in
// the fixture and the comment on ScopeKeys.
package govcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type scopeContractCase struct {
	Name               string   `json:"name"`
	Org                string   `json:"org"`
	Team               string   `json:"team"`
	App                string   `json:"app"`
	CrossDomainEqual   bool     `json:"cross_domain_equal"`
	ScopeKey           string   `json:"scope_key"`
	Chain              []string `json:"chain"`
	ChainGovernance    []string `json:"chain_governance"`
	ScopeKeyGovernance string   `json:"scope_key_governance"`
}

// allowedContractCase drives the allowed_models INTERSECTION rule. Its own local
// struct, decoding only what this side needs — the Core declares a different one for
// the same JSON, which is what "no shared library across domains" (D3) means here.
type allowedContractCase struct {
	Name           string                `json:"name"`
	Levels         []map[string][]string `json:"levels"`
	ExpectDeclared bool                  `json:"expect_declared"`
	Expect         []string              `json:"expect"`
}

type scopeContractFixture struct {
	Cases        []scopeContractCase   `json:"cases"`
	AllowedCases []allowedContractCase `json:"allowed_models_cases"`
}

// fixturePath: from the test's folder (.../governance/src/internal/govcore) up to
// the repository root is 5 levels, then the fixture's path.
const govFixturePath = "../../../../../testdata/contracts/config-scope/scope-chain.json"

func loadScopeFixture(t *testing.T) scopeContractFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(govFixturePath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var f scopeContractFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	return f
}

func TestScopeChainContract_Governance(t *testing.T) {
	f := loadScopeFixture(t)
	for _, c := range f.Cases {
		if got := ScopeKeys(c.Org, c.Team, c.App); !reflect.DeepEqual(got, c.ChainGovernance) {
			t.Errorf("[%s] ScopeKeys(%q,%q,%q)=%v, the contract wants %v", c.Name, c.Org, c.Team, c.App, got, c.ChainGovernance)
		}
		if got := ScopeKey(c.Org, c.Team, c.App); got != c.ScopeKeyGovernance {
			t.Errorf("[%s] ScopeKey(%q,%q,%q)=%q, the contract wants %q", c.Name, c.Org, c.Team, c.App, got, c.ScopeKeyGovernance)
		}
	}
}

// CONTRACT test for the allowed_models INTERSECTION rule — the GOVERNANCE side.
//
// Same fixture, same cases, different implementation: the Core folds them with
// ddbconfig.foldAllowed, this side with IntersectAllowed. The fixture is the only
// thing shared, and it is what stops the two from drifting — the console showing a
// list the gateway refuses is exactly the class of bug the scope-chain divergence
// already produced once (see the fixture's own note).
//
// The nil/empty distinction is the point: a level whose JSON has no `allowed_models`
// key decodes to a nil slice (not declared), while `[]` decodes to a non-nil empty
// slice (declared, and it denies everything).
func TestAllowedModelsIntersectionContract_Governance(t *testing.T) {
	f := loadScopeFixture(t)
	if len(f.AllowedCases) == 0 {
		t.Fatal("fixture has no allowed_models_cases — the contract would pass over nothing")
	}
	for _, c := range f.AllowedCases {
		var acc []string
		for _, level := range c.Levels {
			acc = IntersectAllowed(acc, level["allowed_models"])
		}
		declared := acc != nil
		if declared != c.ExpectDeclared {
			t.Errorf("[%s] declared=%v, the contract wants %v (effective=%v)", c.Name, declared, c.ExpectDeclared, acc)
			continue
		}
		if !declared {
			continue
		}
		want := c.Expect
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(acc, want) {
			t.Errorf("[%s] effective=%v, the contract wants %v", c.Name, acc, want)
		}
	}
}

// IntersectAllowed must not hand back a slice aliasing either input: callers cache
// merged config by reference, so a later write through the result would corrupt it.
func TestIntersectAllowed_DoesNotAliasItsInputs(t *testing.T) {
	parent := []string{"a", "b", "c"}
	child := []string{"a", "b"}

	got := IntersectAllowed(parent, child)
	got[0] = "MUTATED"
	if parent[0] != "a" || child[0] != "a" {
		t.Fatalf("writing to the result mutated an input: parent=%v child=%v", parent, child)
	}
	// The pass-through branches are the ones most likely to be written as a bare return.
	fromChild := IntersectAllowed(nil, child)
	fromChild[0] = "MUTATED"
	if child[0] != "a" {
		t.Fatalf("the parent-nil branch aliased the child: %v", child)
	}
	fromParent := IntersectAllowed(parent, nil)
	fromParent[0] = "MUTATED"
	if parent[0] != "a" {
		t.Fatalf("the child-nil branch aliased the parent: %v", parent)
	}
}

// A declared-empty list must survive as declared-empty. Collapsing it to nil is the
// inversion that made "deny everything" mean "allow everything".
func TestIntersectAllowed_DeclaredEmptyIsNotNil(t *testing.T) {
	if got := IntersectAllowed([]string{"a"}, []string{}); got == nil || len(got) != 0 {
		t.Fatalf("intersecting with a declared empty list must stay declared and empty, got %#v", got)
	}
	if got := IntersectAllowed([]string{"a"}, []string{"b"}); got == nil || len(got) != 0 {
		t.Fatalf("disjoint declarations must deny everything, not allow everything, got %#v", got)
	}
	if got := IntersectAllowed(nil, nil); got != nil {
		t.Fatalf("nothing declared must stay nil (no restriction), got %#v", got)
	}
}
