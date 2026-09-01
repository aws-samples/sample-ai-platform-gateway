// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

import "testing"

const now = "2026-08-12T12:00:00Z"

// Property 6: total name validation.
func TestValidName(t *testing.T) {
	if ValidName("") || ValidName("   ") {
		t.Fatal("an empty name should be invalid")
	}
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if ValidName(long) {
		t.Fatal("a 65-char name should be invalid")
	}
	if !ValidName("Plataforma") || !ValidName("a") {
		t.Fatal("a valid name was rejected")
	}
}

// Property 2: id uniqueness (the slug disambiguates a collision).
func TestSlugIDCollision(t *testing.T) {
	if id := SlugID("Plataforma X", nil); id != "plataforma-x" {
		t.Fatalf("unexpected slug: %q", id)
	}
	taken := map[string]bool{"plataforma": true, "plataforma-2": true}
	if id := SlugID("Plataforma", taken); id != "plataforma-3" {
		t.Fatalf("disambiguation failed: %q", id)
	}
	if id := SlugID("!!!", nil); id != "item" {
		t.Fatalf("an empty slug should become 'item': %q", id)
	}
}

// Property 2: AddTeam refuses an existing id without overwriting.
func TestAddTeamExists(t *testing.T) {
	tree, err := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddTeam(tree, "plataforma", "Outra", now); err != ErrExists {
		t.Fatalf("expected ErrExists, got %v", err)
	}
	// the original display_name was not overwritten
	if tree.Teams["plataforma"].DisplayName != "Plataforma" {
		t.Fatal("display_name was overwritten")
	}
}

func TestAddTeamInvalidName(t *testing.T) {
	if _, err := AddTeam(OrgTree{}, "x", "  ", now); err != ErrInvalidName {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
}

// Property 1: a rename changes only display_name and preserves id/created_at.
func TestRenameTeamStableID(t *testing.T) {
	tree, _ := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	renamed, err := RenameTeam(tree, "plataforma", "Core Team")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := renamed.Teams["plataforma"]
	if !ok {
		t.Fatal("the id changed on rename")
	}
	if m.DisplayName != "Core Team" {
		t.Fatalf("display_name did not update: %q", m.DisplayName)
	}
	if m.CreatedAt != now {
		t.Fatal("created_at was changed on rename")
	}
	if _, err := RenameTeam(tree, "inexistente", "X"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Property 3: referential integrity — an app requires an existing team.
func TestAddAppRequiresTeam(t *testing.T) {
	if _, err := AddApp(OrgTree{}, "checkout", "plataforma", "Checkout", now); err != ErrTeamNotFound {
		t.Fatalf("expected ErrTeamNotFound, got %v", err)
	}
	tree, _ := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	tree, err := AddApp(tree, "checkout", "plataforma", "Checkout", now)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Apps["checkout"].Team != "plataforma" {
		t.Fatal("the app was not bound to the team")
	}
	if _, err := AddApp(tree, "checkout", "plataforma", "Dup", now); err != ErrExists {
		t.Fatalf("expected ErrExists, got %v", err)
	}
}

// Property 3: removing a team that still has an active app is refused.
func TestRemoveTeamWithApps(t *testing.T) {
	tree, _ := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	tree, _ = AddApp(tree, "checkout", "plataforma", "Checkout", now)
	if _, err := RemoveTeam(tree, "plataforma"); err != ErrHasApps {
		t.Fatalf("expected ErrHasApps, got %v", err)
	}
	// removing the app first lets the team go
	tree, _ = RemoveApp(tree, "checkout")
	tree, err := RemoveTeam(tree, "plataforma")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.Teams["plataforma"]; ok {
		t.Fatal("the team was not removed")
	}
}

// Property 4: archiving is idempotent.
func TestArchiveIdempotent(t *testing.T) {
	tree, _ := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	tree, err := SetTeamStatus(tree, "plataforma", StatusArchived)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Teams["plataforma"].Status != StatusArchived {
		t.Fatal("the status did not become archived")
	}
	// applying it again does not fail and keeps archived
	tree2, err := SetTeamStatus(tree, "plataforma", StatusArchived)
	if err != nil {
		t.Fatal(err)
	}
	if tree2.Teams["plataforma"].Status != StatusArchived {
		t.Fatal("idempotency broken")
	}
}

// Property 5: a transition preserves the remaining items (it does not mutate the
// original argument).
func TestPreservationNoMutation(t *testing.T) {
	base, _ := AddTeam(OrgTree{}, "plataforma", "Plataforma", now)
	base, _ = AddTeam(base, "dados", "Dados", now)
	// operating on one team must not affect the other nor mutate the original
	after, _ := RenameTeam(base, "plataforma", "Core")
	if base.Teams["plataforma"].DisplayName != "Plataforma" {
		t.Fatal("the original tree was mutated (aliasing)")
	}
	if after.Teams["dados"].DisplayName != "Dados" {
		t.Fatal("a non-target team was changed")
	}
	if len(after.Teams) != 2 {
		t.Fatal("a team was lost in the transition")
	}
}
