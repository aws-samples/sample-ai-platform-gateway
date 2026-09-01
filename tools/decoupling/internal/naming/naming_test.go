// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package naming

import (
	"strings"
	"testing"
	"testing/quick"
)

// seg generates a valid [a-z0-9]+ segment from arbitrary bytes.
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

// Feature: multi-account-decoupling, Property 1: Naming convention format and idempotence
func TestProperty1_NameFormatIdempotent(t *testing.T) {
	f := func(p, e, r string, di uint8) bool {
		project, env, res := seg(p), seg(e), seg(r)
		dom := doms[int(di)%len(doms)]
		want := project + "-" + env + "-" + dom + "-" + res
		got1 := Name(project, env, dom, res)
		got2 := Name(project, env, dom, res)
		return got1 == want && got1 == got2
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Feature: multi-account-decoupling, Property 2: Injectivity / no collision across environments
func TestProperty2_InjectiveAcrossEnvs(t *testing.T) {
	f := func(p1, e1, p2, e2, r string, di uint8) bool {
		proj1, env1, proj2, env2, res := seg(p1), seg(e1), seg(p2), seg(e2), seg(r)
		dom := doms[int(di)%len(doms)]
		samePair := proj1 == proj2 && env1 == env2
		if samePair {
			return true // equal pairs may collide; the property is about DISTINCT pairs
		}
		return Name(proj1, env1, dom, res) != Name(proj2, env2, dom, res)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

// Feature: multi-account-decoupling, Property 3: Construction and round-trip of an account-dependent identifier
func TestProperty3_AccountDepRoundTrip(t *testing.T) {
	f := func(rg, acc, nm string) bool {
		region, account, name := seg(rg), seg(acc), seg(nm)
		url := QueueURL(region, account, name)
		arn := QueueARN(region, account, name)
		if strings.Contains(url, "111122223333") || strings.Contains(arn, "111122223333") {
			return false
		}
		r1, a1, n1, ok1 := ParseQueueURL(url)
		r2, a2, n2, ok2 := ParseQueueARN(arn)
		return ok1 && ok2 &&
			r1 == region && a1 == account && n1 == name &&
			r2 == region && a2 == account && n2 == name
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatal(err)
	}
}

// Golden of the anchor names: with aiplat/poc, the names MUST be the current live ones.
// Feature: multi-account-decoupling (Requirements 3.3, 3.4, 7.1)
func TestGoldenLiveNames(t *testing.T) {
	cases := map[string]string{
		Name("aiplat", "poc", "inf", "api-keys"):   "aiplat-poc-inf-api-keys",
		Name("aiplat", "poc", "inf", "cache"):      "aiplat-poc-inf-cache",
		Name("aiplat", "poc", "inf", "limits"):     "aiplat-poc-inf-limits",
		Name("aiplat", "poc", "obs", "cost-store"): "aiplat-poc-obs-cost-store",
		Name("aiplat", "poc", "obs", "usage"):      "aiplat-poc-obs-usage",
		Name("aiplat", "poc", "obs", "usage-dlq"):  "aiplat-poc-obs-usage-dlq",
		Name("aiplat", "poc", "obs", "finops"):     "aiplat-poc-obs-finops",
		Name("aiplat", "poc", "gov", "config"):     "aiplat-poc-gov-config",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("name mismatch: got %q want %q", got, want)
		}
	}
}

func TestValidateSegment(t *testing.T) {
	ok := []string{"aiplat", "poc", "prod", "silo01"}
	bad := []string{"a-b", "Aiplat", "po_c", "", "sí"}
	for _, s := range ok {
		if err := ValidateSegment(s); err != nil {
			t.Errorf("expected valid: %q (%v)", s, err)
		}
	}
	for _, s := range bad {
		if err := ValidateSegment(s); err == nil {
			t.Errorf("expected invalid: %q", s)
		}
	}
}
