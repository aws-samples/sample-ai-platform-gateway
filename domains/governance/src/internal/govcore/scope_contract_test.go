// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the scope chain (hexagonal-refactor, task 12, D3, R9.3/9.4,
// Property 5).
//
// Validates GOVERNANCE's govcore.ScopeKeys/ScopeKey against the shared fixture at
// testdata/contracts/hexagonal-refactor/scope-chain.json. The SAME fixture is
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

type scopeContractFixture struct {
	Cases []scopeContractCase `json:"cases"`
}

// fixturePath: from the test's folder (.../governance/src/internal/govcore) up to
// the repository root is 5 levels, then the fixture's path.
const govFixturePath = "../../../../../testdata/contracts/hexagonal-refactor/scope-chain.json"

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
