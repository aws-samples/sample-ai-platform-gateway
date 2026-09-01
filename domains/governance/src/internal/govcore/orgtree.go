// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package govcore

// OrgTree is the registry of ONE org's teams and apps (metadata: display name,
// status, creation). The team's policy (budget/allowed_models) does NOT live here —
// it stays in the ORG#…TEAM#<id> config scope. Here it is only "the team/app
// exists and this is what it is called".
//
// PURE domain (hexagonal, R5): no SDK/network/clock/randomness. The instant and
// the id generator come in as parameters. Verified by boundary_test.
//
// The id (the maps' key) is STABLE: it is what the API key stores (team_id/app),
// what the Core uses to resolve the config scope and what Observability
// aggregates. A rename changes only DisplayName — never the id — so history is
// not broken.

import (
	"errors"
	"strings"
)

// Sentinel errors: the shell maps them to HTTP statuses (409/404/400) without
// inspecting the error string.
var (
	ErrExists       = errors.New("already exists")
	ErrNotFound     = errors.New("not found")
	ErrTeamNotFound = errors.New("team not found")
	ErrHasApps      = errors.New("team still has apps")
	ErrInvalidName  = errors.New("invalid name")
)

// Possible statuses of a team/app.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// TeamMeta is a team's metadata.
type TeamMeta struct {
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// AppMeta is an app's metadata (bound to a team by the team's id).
type AppMeta struct {
	Team        string `json:"team"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// OrgTree is an org's entire registry (a single item in the config store).
type OrgTree struct {
	Teams     map[string]TeamMeta `json:"teams"`
	Apps      map[string]AppMeta  `json:"apps"`
	UpdatedAt string              `json:"updated_at"`
}

// ensure returns a copy with the maps initialized (never nil), so the transitions
// can write without nil checks and without mutating the original argument.
func (t OrgTree) ensure() OrgTree {
	teams := map[string]TeamMeta{}
	for k, v := range t.Teams {
		teams[k] = v
	}
	apps := map[string]AppMeta{}
	for k, v := range t.Apps {
		apps[k] = v
	}
	return OrgTree{Teams: teams, Apps: apps, UpdatedAt: t.UpdatedAt}
}

// ValidName accepts a non-empty name (after trimming) with 1..64 characters.
func ValidName(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len([]rune(s)) <= 64
}

// SlugID derives a stable, readable id from the name (lowercase, [a-z0-9-],
// spaces/symbols become '-'), disambiguating against the ids already taken with a
// numeric suffix. Deterministic — it uses no randomness.
func SlugID(name string, taken map[string]bool) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "item"
	}
	if !taken[base] {
		return base
	}
	// Disambiguate: base-2, base-3, …
	for i := 2; ; i++ {
		cand := base + "-" + itoa(i)
		if !taken[cand] {
			return cand
		}
	}
}

// itoa avoids importing strconv (keeps the domain minimal). i is always > 0 here.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// AddTeam adds a team with an already resolved id. ErrExists when the id is taken.
func AddTeam(t OrgTree, id, displayName, now string) (OrgTree, error) {
	if !ValidName(displayName) {
		return t, ErrInvalidName
	}
	nt := t.ensure()
	if _, ok := nt.Teams[id]; ok {
		return t, ErrExists
	}
	nt.Teams[id] = TeamMeta{DisplayName: strings.TrimSpace(displayName), Status: StatusActive, CreatedAt: now}
	nt.UpdatedAt = now
	return nt, nil
}

// RenameTeam changes only DisplayName; id/scope/history stay untouched.
func RenameTeam(t OrgTree, id, displayName string) (OrgTree, error) {
	if !ValidName(displayName) {
		return t, ErrInvalidName
	}
	nt := t.ensure()
	m, ok := nt.Teams[id]
	if !ok {
		return t, ErrNotFound
	}
	m.DisplayName = strings.TrimSpace(displayName)
	nt.Teams[id] = m
	return nt, nil
}

// SetTeamStatus sets the status (active/archived). Idempotent.
func SetTeamStatus(t OrgTree, id, status string) (OrgTree, error) {
	nt := t.ensure()
	m, ok := nt.Teams[id]
	if !ok {
		return t, ErrNotFound
	}
	m.Status = status
	nt.Teams[id] = m
	return nt, nil
}

// RemoveTeam deletes the team. ErrHasApps when ANY app (active or archived) is
// still bound to it — the caller must remove/move the apps first.
func RemoveTeam(t OrgTree, id string) (OrgTree, error) {
	nt := t.ensure()
	if _, ok := nt.Teams[id]; !ok {
		return t, ErrNotFound
	}
	for _, a := range nt.Apps {
		if a.Team == id {
			return t, ErrHasApps
		}
	}
	delete(nt.Teams, id)
	return nt, nil
}

// AddApp adds an app bound to an existing team. ErrTeamNotFound when the team does
// not exist; ErrExists when the app id is already taken.
func AddApp(t OrgTree, id, team, displayName, now string) (OrgTree, error) {
	if !ValidName(displayName) {
		return t, ErrInvalidName
	}
	nt := t.ensure()
	if _, ok := nt.Teams[team]; !ok {
		return t, ErrTeamNotFound
	}
	if _, ok := nt.Apps[id]; ok {
		return t, ErrExists
	}
	nt.Apps[id] = AppMeta{Team: team, DisplayName: strings.TrimSpace(displayName), Status: StatusActive, CreatedAt: now}
	nt.UpdatedAt = now
	return nt, nil
}

// RenameApp changes only the app's DisplayName.
func RenameApp(t OrgTree, id, displayName string) (OrgTree, error) {
	if !ValidName(displayName) {
		return t, ErrInvalidName
	}
	nt := t.ensure()
	m, ok := nt.Apps[id]
	if !ok {
		return t, ErrNotFound
	}
	m.DisplayName = strings.TrimSpace(displayName)
	nt.Apps[id] = m
	return nt, nil
}

// SetAppStatus sets the app's status (active/archived). Idempotent.
func SetAppStatus(t OrgTree, id, status string) (OrgTree, error) {
	nt := t.ensure()
	m, ok := nt.Apps[id]
	if !ok {
		return t, ErrNotFound
	}
	m.Status = status
	nt.Apps[id] = m
	return nt, nil
}

// RemoveApp deletes the app.
func RemoveApp(t OrgTree, id string) (OrgTree, error) {
	nt := t.ensure()
	if _, ok := nt.Apps[id]; !ok {
		return t, ErrNotFound
	}
	delete(nt.Apps, id)
	return nt, nil
}

// TakenTeamIDs / TakenAppIDs help the shell build the set for SlugID.
func TakenTeamIDs(t OrgTree) map[string]bool {
	s := map[string]bool{}
	for k := range t.Teams {
		s[k] = true
	}
	return s
}

func TakenAppIDs(t OrgTree) map[string]bool {
	s := map[string]bool{}
	for k := range t.Apps {
		s[k] = true
	}
	return s
}
