// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import (
	"strings"
	"testing"
)

func TestRetentionDays(t *testing.T) {
	if RetentionDays != 365 {
		t.Errorf("RetentionDays = %d, expected 365", RetentionDays)
	}
}

// Role × operator matrix. Single-client deployment: the gate is role only, and it
// applies ON THE SERVER — a gate only on the screen falls to a curl.
func TestCanRead_MatrizCompleta(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		operator bool
		wantOK   bool
		wantWhy  string
	}{
		{"owner", RoleOwner, false, true, ""},
		{"admin", RoleAdmin, false, true, ""},
		{"dev", RoleDev, false, false, DenyRole},
		{"billing", RoleBilling, false, false, DenyRole},
		{"platform_admin role", RolePlatformAdmin, false, true, ""},
		{"operator via flag", RoleDev, true, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := CanRead(tc.role, tc.operator)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, expected %v", ok, tc.wantOK)
			}
			if why != tc.wantWhy {
				t.Errorf("reason = %q, expected %q", why, tc.wantWhy)
			}
		})
	}
}

func TestActorTypeFor(t *testing.T) {
	if ActorTypeFor(RolePlatformAdmin) != ActorOperator {
		t.Error("platform_admin should be an operator")
	}
	for _, r := range []string{RoleOwner, RoleAdmin, RoleBilling, RoleDev, ""} {
		if ActorTypeFor(r) != ActorCustomer {
			t.Errorf("role %q should be a customer", r)
		}
	}
}

func TestNewActor_NormalizaEmail(t *testing.T) {
	a := NewActor("  Admin@Corp.COM ", "sub-1", RoleAdmin)
	if a.Email != "admin@corp.com" {
		t.Errorf("e-mail not normalized: %q", a.Email)
	}
	if a.Type != ActorCustomer {
		t.Errorf("type = %q", a.Type)
	}
}

func TestSortKeys_FormatoEPrefixos(t *testing.T) {
	sk, catSK, actorSK := SortKeys("2026-08-13T22:39:40Z", "01J8ABC", CatConfig, "TestUser@Example.com")

	if sk != "TS#2026-08-13T22:39:40Z#01J8ABC" {
		t.Errorf("sk = %q", sk)
	}
	// The prefix built by CategoryPrefix must match the real catSK, otherwise the query
	// by category silently returns empty.
	if !strings.HasPrefix(catSK, CategoryPrefix(CatConfig)) {
		t.Errorf("catSK %q does not match the prefix %q", catSK, CategoryPrefix(CatConfig))
	}
	if !strings.HasPrefix(actorSK, ActorPrefix("testuser@example.com")) {
		t.Errorf("actorSK %q does not match the prefix", actorSK)
	}
	// The actor's e-mail is normalized in the key: without that, "A@b.com" and "a@b.com"
	// would become two different people in the filter.
	if strings.Contains(actorSK, "TestUser") {
		t.Errorf("actorSK should normalize the e-mail: %q", actorSK)
	}
}

// The eventID in the suffix is what avoids a collision under concurrency: without it, two
// events at the same instant would have the SAME key and the conditional write would
// discard the second as a duplicate — losing a legitimate record.
func TestSortKeys_EventIDEvitaColisaoNoMesmoInstante(t *testing.T) {
	ts := "2026-08-13T22:39:40Z"
	a, _, _ := SortKeys(ts, "evt-1", CatConfig, "x@y.com")
	b, _, _ := SortKeys(ts, "evt-2", CatConfig, "x@y.com")
	if a == b {
		t.Fatal("distinct events at the same instant must not produce the same key")
	}
}

func TestPartitionKey(t *testing.T) {
	if got := PartitionKey("org_abc"); got != "AUDIT#org_abc" {
		t.Errorf("PartitionKey = %q", got)
	}
}
