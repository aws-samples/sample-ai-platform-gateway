// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// CONTRACT test for the narrow-only ceiling rule — the GOVERNANCE (write) side
// (GOV-SEC-001). It drives the SAME ceiling_cases the Core folds on read
// (core/internal/adapters/ddbconfig/scope_contract_test.go), asserting
// CeilingViolations flags exactly the keys the fixture marks as widening. No shared
// code across domains (D3); the fixture is what keeps the two in step.
package govcore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type ceilingContractCase struct {
	Name     string                 `json:"name"`
	BelowOrg bool                   `json:"below_org"`
	Parent   map[string]interface{} `json:"parent"`
	Child    map[string]interface{} `json:"child"`
	Widens   []string               `json:"widens"`
}

type ceilingFixture struct {
	CeilingCases []ceilingContractCase `json:"ceiling_cases"`
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func TestCeilingViolationsContract_Governance(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(govFixturePath))
	if err != nil {
		t.Fatalf("could not read the contract fixture: %v", err)
	}
	var f ceilingFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	if len(f.CeilingCases) == 0 {
		t.Fatal("fixture has no ceiling_cases — the contract would pass over nothing")
	}
	for _, c := range f.CeilingCases {
		got := CeilingViolations(c.Parent, c.Child, c.BelowOrg)
		if !sameSet(got, c.Widens) {
			t.Errorf("[%s] CeilingViolations = %v, the contract wants %v", c.Name, got, c.Widens)
		}
	}
}
