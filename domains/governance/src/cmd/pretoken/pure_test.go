// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Tests the PURE piece of pretoken extracted in task 13.3 (thin shell, no ports).
package main

import (
	"reflect"
	"testing"
)

func TestClaimsFromMeta(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]interface{}
		want map[string]string
	}{
		{
			"team + apps",
			map[string]interface{}{"team": "sre", "apps": []interface{}{"api", "web"}},
			map[string]string{"team": "sre", "apps": "api,web"},
		},
		{
			"team only",
			map[string]interface{}{"team": "sre"},
			map[string]string{"team": "sre"},
		},
		{
			"empty team and empty apps → nothing (unrestricted owner/admin)",
			map[string]interface{}{"team": "", "apps": []interface{}{}},
			map[string]string{},
		},
		{
			"empty app entries are ignored",
			map[string]interface{}{"apps": []interface{}{"", "api", ""}},
			map[string]string{"apps": "api"},
		},
		{
			"nil meta → nothing",
			nil,
			map[string]string{},
		},
	}
	for _, c := range cases {
		if got := claimsFromMeta(c.meta); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: claimsFromMeta=%v, want %v", c.name, got, c.want)
		}
	}
}
