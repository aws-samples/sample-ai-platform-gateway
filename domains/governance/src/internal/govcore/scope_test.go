// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import (
	"reflect"
	"testing"
)

func TestScopeKey(t *testing.T) {
	cases := []struct{ org, team, app, want string }{
		{"", "", "", "global"},
		{"acme", "", "", "ORG#acme"},
		{"acme", "sre", "", "ORG#acme#TEAM#sre"},
		{"acme", "sre", "api", "ORG#acme#TEAM#sre#APP#api"},
		{"acme", "", "api", "ORG#acme#TEAM#default#APP#api"},
	}
	for _, c := range cases {
		if got := ScopeKey(c.org, c.team, c.app); got != c.want {
			t.Errorf("ScopeKey(%q,%q,%q)=%q, want %q", c.org, c.team, c.app, got, c.want)
		}
	}
}

func TestScopeKeys(t *testing.T) {
	cases := []struct {
		org, team, app string
		want           []string
	}{
		{"", "", "", []string{"global"}},
		// org without a team: the CHAIN (merge) includes TEAM#default, aligned with
		// the Core — it is what the gateway applies (team resolves to default). The
		// write TARGET (ScopeKey) is still ORG#acme, covered by TestScopeKey.
		{"acme", "", "", []string{"global", "ORG#acme", "ORG#acme#TEAM#default"}},
		{"acme", "sre", "", []string{"global", "ORG#acme", "ORG#acme#TEAM#sre"}},
		{"acme", "sre", "api", []string{"global", "ORG#acme", "ORG#acme#TEAM#sre", "ORG#acme#TEAM#sre#APP#api"}},
		{"acme", "", "api", []string{"global", "ORG#acme", "ORG#acme#TEAM#default", "ORG#acme#TEAM#default#APP#api"}},
	}
	for _, c := range cases {
		if got := ScopeKeys(c.org, c.team, c.app); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ScopeKeys(%q,%q,%q)=%v, want %v", c.org, c.team, c.app, got, c.want)
		}
	}
}

// Property: "global" is always the first link, and the write TARGET (ScopeKey) is
// always a MEMBER of the merge chain.
//
// The invariant used to be "last of the chain == ScopeKey", but it stopped holding
// on purpose in the org-without-team case: the chain (plural, for the effective
// MERGE) goes up to ORG#…#TEAM#default, while ScopeKey (singular, the write
// TARGET) stops at ORG#. They are different concepts — merge vs write — and the
// write point still takes part in the merge, so the correct invariant is
// membership.
func TestScopeChainConsistency(t *testing.T) {
	inputs := []struct{ org, team, app string }{
		{"", "", ""}, {"o", "", ""}, {"o", "t", ""}, {"o", "t", "a"}, {"o", "", "a"},
	}
	for _, in := range inputs {
		chain := ScopeKeys(in.org, in.team, in.app)
		if chain[0] != "global" {
			t.Errorf("the chain does not start at global: %v", chain)
		}
		key := ScopeKey(in.org, in.team, in.app)
		found := false
		for _, k := range chain {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ScopeKey %q is not in the chain %v for %+v", key, chain, in)
		}
	}
}

func TestDeepMerge(t *testing.T) {
	dst := map[string]interface{}{
		"cache_ttl": 15,
		"routing":   map[string]interface{}{"auto_cheapest": true, "models": []interface{}{"a"}},
	}
	src := map[string]interface{}{
		"cache_ttl": 60,
		"routing":   map[string]interface{}{"models": []interface{}{"b"}},
		"plan":      "pro",
	}
	DeepMerge(dst, src)
	want := map[string]interface{}{
		"cache_ttl": 60,
		"routing":   map[string]interface{}{"auto_cheapest": true, "models": []interface{}{"b"}},
		"plan":      "pro",
	}
	if !reflect.DeepEqual(dst, want) {
		t.Errorf("DeepMerge:\n got=%#v\nwant=%#v", dst, want)
	}
}
