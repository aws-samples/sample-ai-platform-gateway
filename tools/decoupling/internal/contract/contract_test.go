// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package contract

import (
	"strings"
	"testing"
	"testing/quick"
)

func seg(raw string) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for _, r := range raw {
		b.WriteByte(alpha[int(r)%len(alpha)])
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

var doms = []string{"inf", "obs", "gov", "fe", "bo"}

// Feature: multi-account-decoupling, Property 4: Round-trip build/parse of the Contract path
func TestProperty4_PathRoundTrip(t *testing.T) {
	f := func(p, e, k string, di uint8) bool {
		project, env, key := seg(p), seg(e), seg(k)
		dom := doms[int(di)%len(doms)]
		path := BuildPath(project, env, dom, key)
		if !strings.HasPrefix(path, "/"+project+"/"+env+"/") {
			return false
		}
		gp, ge, gd, gk, err := ParsePath(path)
		return err == nil && gp == project && ge == env && gd == dom && gk == key
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Feature: multi-account-decoupling, Property 2 (paths): Injectivity / no collision across environments
func TestProperty2_PathInjectiveAcrossEnvs(t *testing.T) {
	f := func(p1, e1, p2, e2, k string, di uint8) bool {
		proj1, env1, proj2, env2, key := seg(p1), seg(e1), seg(p2), seg(e2), seg(k)
		dom := doms[int(di)%len(doms)]
		if proj1 == proj2 && env1 == env2 {
			return true
		}
		return BuildPath(proj1, env1, dom, key) != BuildPath(proj2, env2, dom, key)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Feature: multi-account-decoupling, Property 5: override > contract precedence in resolution
func TestProperty5_ResolvePrecedence(t *testing.T) {
	f := func(ov, ssm string) bool {
		override, ssmVal := seg(ov), seg(ssm)
		// non-empty override wins
		if Resolve(&override, &ssmVal) != override {
			return false
		}
		// nil override -> ssm applies
		if Resolve(nil, &ssmVal) != ssmVal {
			return false
		}
		// empty override -> treated as absence -> ssm applies
		empty := ""
		return Resolve(&empty, &ssmVal) == ssmVal
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Feature: multi-account-decoupling, Property 6: The Apply Order is a topological ordering with unique positions
func TestProperty6_TopoOrder(t *testing.T) {
	pos, err := TopoOrder(Domains, DefaultGraph)
	if err != nil {
		t.Fatal(err)
	}
	// (b) permutation of 1..N: unique positions covering 1..N
	seen := map[int]bool{}
	for _, n := range Domains {
		p, ok := pos[n]
		if !ok {
			t.Fatalf("domain without position: %s", n)
		}
		if p < 1 || p > len(Domains) || seen[p] {
			t.Fatalf("invalid or repeated position: %s=%d", n, p)
		}
		seen[p] = true
	}
	// (a) for every edge, pos(producer) < pos(consumer)
	for _, e := range DefaultGraph {
		if pos[e.Producer] >= pos[e.Consumer] {
			t.Fatalf("order violated: %s(%d) must come before %s(%d)", e.Producer, pos[e.Producer], e.Consumer, pos[e.Consumer])
		}
	}
	// governance must be first
	if pos["governance"] != 1 {
		t.Fatalf("governance should be position 1, was %d", pos["governance"])
	}
}

// A cycle must be detected.
func TestTopoOrderCycleDetected(t *testing.T) {
	edges := []Edge{{"a", "b"}, {"b", "a"}}
	if _, err := TopoOrder([]string{"a", "b"}, edges); err == nil {
		t.Fatal("expected a cycle error")
	}
}

// The order is a valid topo-order: governance first; frontend after
// core and observability (its dependencies). backoffice only depends on governance,
// so it can float — it is not forced to be last.
func TestApplyOrderMatchesDesign(t *testing.T) {
	order, err := ApplyOrder(Domains, DefaultGraph)
	if err != nil {
		t.Fatal(err)
	}
	idx := map[string]int{}
	for i, d := range order {
		idx[d] = i
	}
	if order[0] != "governance" {
		t.Fatalf("first must be governance, was %s", order[0])
	}
	if !(idx["frontend"] > idx["core"] && idx["frontend"] > idx["observability"]) {
		t.Fatalf("frontend must come after core and observability, order=%v", order)
	}
	if !(idx["core"] > idx["governance"] && idx["observability"] > idx["governance"] && idx["backoffice"] > idx["governance"]) {
		t.Fatalf("core/observability/backoffice must come after governance, order=%v", order)
	}
}

func TestMissingParamError(t *testing.T) {
	msg := MissingParamError("aiplat", "poc", "governance/cognito_client_id")
	if !strings.Contains(msg, "/aiplat/poc/governance/cognito_client_id") || !strings.Contains(msg, "governance") {
		t.Fatalf("unexpected message: %s", msg)
	}
}
