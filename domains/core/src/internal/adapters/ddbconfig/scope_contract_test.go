// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the scope chain — the CORE side (hexagonal-refactor,
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

type scopeContractFixture struct {
	Cases []scopeContractCase `json:"cases"`
}

// From the test's folder (.../core/src/internal/adapters/ddbconfig) to the repository
// root is 6 levels.
const coreFixturePath = "../../../../../../testdata/contracts/hexagonal-refactor/scope-chain.json"

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
