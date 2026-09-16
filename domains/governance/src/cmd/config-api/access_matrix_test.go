// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/aiplat/governance/internal/govcore"
)

// fakeStore turns a map of scope key -> config document into a matrixScopeReader,
// so the assembly is tested without DynamoDB. An absent key returns nil, which is
// what readScope does for a scope that has never been written.
func fakeStore(docs map[string]map[string]interface{}) matrixScopeReader {
	return func(_ context.Context, key string) map[string]interface{} {
		return docs[key]
	}
}

func list(v ...interface{}) []interface{} { return v }

// ── absent is not the same as declared-empty ──────────────────────────────────
// The chain resolves allowed_models by INTERSECTION, which can legitimately produce
// the empty set (a team allowing [a] under an app allowing [b]). The gateway reads a
// DECLARED empty list as "deny everything" and a nil one as "no restriction", so
// collapsing the two here would have the matrix draw full access where the gateway
// serves nothing — or the reverse, which is how "turn every model off" used to grant
// every model.
func TestAllowedFrom_DistinguishesAbsentFromDeclaredEmpty(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]interface{}
		want []string
	}{
		{"absent key", map[string]interface{}{}, nil},
		{"wrong type is not a declaration", map[string]interface{}{"allowed_models": "claude"}, nil},
		{"declared empty", map[string]interface{}{"allowed_models": list()}, []string{}},
		{"declared with only blanks is declared empty", map[string]interface{}{"allowed_models": list("", "")}, []string{}},
		{"declared, sorted", map[string]interface{}{"allowed_models": list("nova", "claude")}, []string{"claude", "nova"}},
		// A list ALREADY RESOLVED and written back into the map is []string, not
		// []interface{}. A single type assertion reported that as "not declared",
		// which reset the ceiling halfway through the ?effective=1 chain — a live
		// defect the contract test could not see, because it never goes through a map.
		{"an already-resolved []string is still a declaration",
			map[string]interface{}{"allowed_models": []string{"nova", "claude"}}, []string{"claude", "nova"}},
		{"an already-resolved empty []string is declared empty",
			map[string]interface{}{"allowed_models": []string{}}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := allowedFrom(tc.cfg)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("nil-ness differs: got %#v, want %#v", got, tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBeyond(t *testing.T) {
	if got := beyond([]string{"a", "b", "c"}, []string{"a"}); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("got %v", got)
	}
	if got := beyond([]string{"a"}, []string{"a", "b"}); got != nil {
		t.Fatalf("a subset must not be reported as over the ceiling, got %v", got)
	}
	// No ceiling declared above means nothing can be over it. Without this guard a
	// scope under an unrestricted parent would have its whole list reported as
	// leftover.
	if got := beyond([]string{"a", "b"}, nil); got != nil {
		t.Fatalf("with no ceiling declared, nothing is over it, got %v", got)
	}
}

func TestClipToCatalog(t *testing.T) {
	if got := clipToCatalog([]string{"a", "gone", "b"}, []string{"a", "b", "c"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
	// nil in, nil out: clipping must not turn "no restriction" into "deny everything".
	if got := clipToCatalog(nil, []string{"a"}); got != nil {
		t.Fatalf("nil must stay nil, got %#v", got)
	}
	// A list of nothing but stale aliases becomes a DECLARED empty list, which is
	// honest: the scope restricts to models that no longer exist, so it serves none.
	got := clipToCatalog([]string{"gone"}, []string{"a"})
	if got == nil || len(got) != 0 {
		t.Fatalf("want a declared empty list, got %#v", got)
	}
}

// ── a child cannot widen its parent ───────────────────────────────────────────
// The team allows only claude-sonnet; the app declares three models. The ceiling
// wins: the app is served the intersection, and the two extra models are reported as
// a leftover declaration rather than as access.
func TestBuildAccessMatrix_AppCannotWidenItsTeam(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"global": {"auto_cheapest": true},
		"ORG#acme": {"routing": map[string]interface{}{
			"claude-opus": map[string]interface{}{}, "claude-sonnet": map[string]interface{}{},
			"claude-haiku": map[string]interface{}{}, "nova-lite": map[string]interface{}{},
		}},
		"ORG#acme#TEAM#checkout":                {"allowed_models": list("claude-sonnet")},
		"ORG#acme#TEAM#checkout#APP#batch-jobs": {"allowed_models": list("claude-sonnet", "claude-haiku", "nova-lite")},
	}
	tree := govcore.OrgTree{
		Teams: map[string]govcore.TeamMeta{"checkout": {DisplayName: "Checkout", Status: govcore.StatusActive}},
		Apps: map[string]govcore.AppMeta{
			"batch-jobs": {Team: "checkout", DisplayName: "Batch jobs", Status: govcore.StatusActive},
			"web":        {Team: "checkout", DisplayName: "Web", Status: govcore.StatusActive},
		},
	}

	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", tree, "")

	if !reflect.DeepEqual(mx.Models, []string{"claude-haiku", "claude-opus", "claude-sonnet", "nova-lite"}) {
		t.Fatalf("catalog = %v", mx.Models)
	}

	rows := map[string]accessRow{}
	for _, r := range mx.Rows {
		rows[r.Kind+":"+r.ID] = r
	}

	// The org declares nothing, so it is served the whole catalog and is its own ceiling.
	if org := rows["org:acme"]; org.HasOwn || !reflect.DeepEqual(org.Effective, mx.Models) {
		t.Fatalf("org row = %+v", org)
	}
	// The team restricts.
	if tm := rows["team:checkout"]; !tm.HasOwn || !reflect.DeepEqual(tm.Effective, []string{"claude-sonnet"}) {
		t.Fatalf("team row = %+v", tm)
	}
	// An app with no list of its own INHERITS — it must not be reported as owning
	// the list, because that conflation is what let the console light every pill.
	web := rows["app:web"]
	if web.HasOwn {
		t.Fatalf("web declares nothing, HasOwn must be false: %+v", web)
	}
	if !reflect.DeepEqual(web.Effective, []string{"claude-sonnet"}) ||
		!reflect.DeepEqual(web.ParentEffective, []string{"claude-sonnet"}) {
		t.Fatalf("web row = %+v", web)
	}
	// The app that declares three models under a team allowing one. It is SERVED
	// only the intersection — the team is a ceiling, not a default. This assertion
	// was the opposite before the merge changed: it read
	// [claude-haiku claude-sonnet nova-lite], because a child's list simply replaced
	// its parent's and the gateway really did serve the two extra models.
	bj := rows["app:batch-jobs"]
	if !reflect.DeepEqual(bj.Effective, []string{"claude-sonnet"}) {
		t.Fatalf("batch-jobs effective = %v, want only the intersection [claude-sonnet]", bj.Effective)
	}
	// The two extra models stay visible as a leftover DECLARATION so an operator can
	// remove them; they are just not access any more.
	if !reflect.DeepEqual(bj.OverParent, []string{"claude-haiku", "nova-lite"}) {
		t.Fatalf("batch-jobs over_parent = %v", bj.OverParent)
	}
	if len(mx.Drift) != 1 || mx.Drift[0].ID != "batch-jobs" {
		t.Fatalf("drift = %+v", mx.Drift)
	}
}

// An app declaring ONLY models its team denies must be served NOTHING. Under the old
// replace rule it was served exactly those models; if the empty intersection were
// read as "no restriction" it would instead be served the entire catalog — two
// restrictions producing full access, the worst direction for this to fail in.
func TestBuildAccessMatrix_DisjointDeclarationServesNothing(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme": {"routing": map[string]interface{}{
			"a": map[string]interface{}{}, "b": map[string]interface{}{},
		}},
		"ORG#acme#TEAM#t":       {"allowed_models": list("a")},
		"ORG#acme#TEAM#t#APP#x": {"allowed_models": list("b")},
	}
	tree := govcore.OrgTree{
		Teams: map[string]govcore.TeamMeta{"t": {}},
		Apps:  map[string]govcore.AppMeta{"x": {Team: "t"}},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", tree, "")
	var app accessRow
	for _, r := range mx.Rows {
		if r.Kind == "app" {
			app = r
		}
	}
	if app.Effective == nil {
		t.Fatal("effective must be a DECLARED empty list (deny all), not nil (no restriction)")
	}
	if len(app.Effective) != 0 {
		t.Fatalf("effective = %v, want empty", app.Effective)
	}
	if !reflect.DeepEqual(app.OverParent, []string{"b"}) {
		t.Fatalf("over_parent = %v, want [b]", app.OverParent)
	}
}

// A stale alias — still named in a scope's list, no longer a route — must not become
// a phantom warning. The catalog defines the columns, so a model outside it has no
// cell to click, and it cannot be served either way.
func TestBuildAccessMatrix_StaleAliasIsClippedToTheCatalog(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme": {
			"routing":        map[string]interface{}{"a": map[string]interface{}{}},
			"allowed_models": list("a", "deleted-alias"),
		},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", govcore.OrgTree{}, "")
	org := mx.Rows[0]
	if !reflect.DeepEqual(org.Own, []string{"a"}) {
		t.Fatalf("own = %v, want [a] — the stale alias must be clipped", org.Own)
	}
	if len(org.OverParent) != 0 {
		t.Fatalf("over_parent = %v, want empty: a model with no route is not a grant", org.OverParent)
	}
	if len(mx.Drift) != 0 {
		t.Fatalf("drift = %+v, want none", mx.Drift)
	}
}

// ── the org's OWN list must be read from ORG#<org>, and must become the ceiling ──
//
// Regression test for a bug that shipped: the org's ancestry was taken from
// govcore.ScopeKeys(org,"",""), which appends ORG#<org>#TEAM#default. Reading that
// tail as "the org's own config" made the org's allowed_models invisible
// (has_own=false with the list plainly stored), and orgEff fell back to the whole
// catalog — so every team was shown a ceiling MORE PERMISSIVE than the org's real
// one, which is the single guarantee the matrix exists to provide.
//
// Every other test here has the org declaring nothing, which is exactly why none of
// them caught it.
func TestBuildAccessMatrix_OrgOwnListIsReadAndBecomesTheCeiling(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme": {
			"routing": map[string]interface{}{
				"m1": map[string]interface{}{}, "m2": map[string]interface{}{}, "m3": map[string]interface{}{},
			},
			"allowed_models": list("m1", "m2"),
		},
		// A SIBLING scope, not an ancestor of the org. If it is ever merged into the
		// org row, m3 leaks back into the ceiling and this test fails.
		"ORG#acme#TEAM#default": {"allowed_models": list("m3")},
	}
	tree := govcore.OrgTree{
		Teams: map[string]govcore.TeamMeta{"checkout": {}},
		Apps:  map[string]govcore.AppMeta{"web": {Team: "checkout"}},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", tree, "")

	rows := map[string]accessRow{}
	for _, r := range mx.Rows {
		rows[r.Kind+":"+r.ID] = r
	}

	org := rows["org:acme"]
	if !org.HasOwn {
		t.Fatal("the org declares allowed_models; has_own must be true")
	}
	if !reflect.DeepEqual(org.Own, []string{"m1", "m2"}) {
		t.Fatalf("org own = %v, want [m1 m2]", org.Own)
	}
	if !reflect.DeepEqual(org.Effective, []string{"m1", "m2"}) {
		t.Fatalf("org effective = %v, want [m1 m2]", org.Effective)
	}
	// The catalog stays the full routing table — a restriction narrows what is
	// SERVED, it does not remove a model from the org's catalog.
	if !reflect.DeepEqual(mx.Models, []string{"m1", "m2", "m3"}) {
		t.Fatalf("catalog = %v", mx.Models)
	}
	// The point of the whole fix: a team that declares nothing inherits the ORG's
	// restriction as its ceiling, not the catalog.
	tm := rows["team:checkout"]
	if tm.HasOwn {
		t.Fatalf("team declares nothing: %+v", tm)
	}
	if !reflect.DeepEqual(tm.ParentEffective, []string{"m1", "m2"}) {
		t.Fatalf("team ceiling = %v, want [m1 m2] (the org's list, not the catalog)", tm.ParentEffective)
	}
	if !reflect.DeepEqual(tm.Effective, []string{"m1", "m2"}) {
		t.Fatalf("team effective = %v", tm.Effective)
	}
	// …and it propagates one level further down.
	app := rows["app:web"]
	if !reflect.DeepEqual(app.ParentEffective, []string{"m1", "m2"}) {
		t.Fatalf("app ceiling = %v, want [m1 m2]", app.ParentEffective)
	}
	// TEAM#default declares m3, which the org denies. It is a real scope, so it is
	// listed — never folded into the org row — and under intersection it is served
	// NOTHING: its only declared model is outside the org's ceiling. m3 remains
	// reported as a leftover declaration.
	def := rows["team:default"]
	if def.Effective == nil || len(def.Effective) != 0 {
		t.Fatalf("team default effective = %#v, want a declared empty list (deny all)", def.Effective)
	}
	if !reflect.DeepEqual(def.OverParent, []string{"m3"}) {
		t.Fatalf("team default over_parent = %v, want [m3]", def.OverParent)
	}
}

// Hierarchy order is what makes the matrix readable: a team is immediately
// followed by its own apps, never interleaved with another team's.
func TestBuildAccessMatrix_RowOrderIsHierarchical(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme": {"routing": map[string]interface{}{"m1": map[string]interface{}{}}},
	}
	tree := govcore.OrgTree{
		Teams: map[string]govcore.TeamMeta{"support": {}, "checkout": {}},
		Apps: map[string]govcore.AppMeta{
			"helpdesk": {Team: "support"}, "web": {Team: "checkout"}, "batch": {Team: "checkout"},
		},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", tree, "")
	var got []string
	for _, r := range mx.Rows {
		got = append(got, r.Kind+":"+r.ID)
	}
	want := []string{"org:acme", "team:checkout", "app:batch", "app:web", "team:support", "app:helpdesk"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A team-scoped caller must get only its own team's rows. Without this the
// aggregation would leak the whole org's policy in one call.
func TestBuildAccessMatrix_TeamFilterExcludesOtherTeams(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme": {"routing": map[string]interface{}{"m1": map[string]interface{}{}}},
	}
	tree := govcore.OrgTree{
		Teams: map[string]govcore.TeamMeta{"support": {}, "checkout": {}},
		Apps:  map[string]govcore.AppMeta{"helpdesk": {Team: "support"}, "web": {Team: "checkout"}},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", tree, "checkout")
	for _, r := range mx.Rows {
		if r.Kind == "team" && r.ID != "checkout" {
			t.Fatalf("leaked team row %q", r.ID)
		}
		if r.Kind == "app" && r.Team != "checkout" {
			t.Fatalf("leaked app row %q of team %q", r.ID, r.Team)
		}
	}
}

// `default` is implicit: the merge chain always passes through TEAM#default. Show
// it when something actually resolves there, hide it when it is inert — an empty
// row invites configuring a scope the org does not use.
func TestBuildAccessMatrix_DefaultTeamOnlyWhenItMeansSomething(t *testing.T) {
	base := map[string]interface{}{"routing": map[string]interface{}{"m1": map[string]interface{}{}}}

	inert := buildAccessMatrix(context.Background(),
		fakeStore(map[string]map[string]interface{}{"ORG#acme": base}),
		"acme", govcore.OrgTree{}, "")
	for _, r := range inert.Rows {
		if r.Kind == "team" {
			t.Fatalf("inert default team should not be a row: %+v", r)
		}
	}

	// An app with no team resolves under TEAM#default, so the row has to appear.
	withApp := buildAccessMatrix(context.Background(),
		fakeStore(map[string]map[string]interface{}{"ORG#acme": base}),
		"acme", govcore.OrgTree{Apps: map[string]govcore.AppMeta{"legacy": {Team: ""}}}, "")
	var kinds []string
	for _, r := range withApp.Rows {
		kinds = append(kinds, r.Kind+":"+r.ID)
	}
	want := []string{"org:acme", "team:default", "app:legacy"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("rows = %v, want %v", kinds, want)
	}
}

// A scope with a policy of its own but no registry entry still applies at
// runtime, so it must be listed.
func TestBuildAccessMatrix_UnregisteredTeamWithPolicyIsListed(t *testing.T) {
	docs := map[string]map[string]interface{}{
		"ORG#acme":              {"routing": map[string]interface{}{"m1": map[string]interface{}{}, "m2": map[string]interface{}{}}},
		"ORG#acme#TEAM#default": {"allowed_models": list("m1")},
	}
	mx := buildAccessMatrix(context.Background(), fakeStore(docs), "acme", govcore.OrgTree{}, "")
	found := false
	for _, r := range mx.Rows {
		if r.Kind == "team" && r.ID == "default" {
			found = true
			if !r.HasOwn || !reflect.DeepEqual(r.Effective, []string{"m1"}) {
				t.Fatalf("default row = %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("a team with its own policy must be listed even when unregistered")
	}
}

func TestBuildAccessMatrix_TruncatesInsteadOfTimingOut(t *testing.T) {
	tree := govcore.OrgTree{Teams: map[string]govcore.TeamMeta{}, Apps: map[string]govcore.AppMeta{}}
	for i := 0; i < maxMatrixScopes+50; i++ {
		tree.Teams["t"+itoa(i)] = govcore.TeamMeta{}
	}
	mx := buildAccessMatrix(context.Background(),
		fakeStore(map[string]map[string]interface{}{"ORG#acme": {}}), "acme", tree, "")
	if !mx.Truncated {
		t.Fatal("an org past the ceiling must be reported as truncated")
	}
	if mx.Scopes > maxMatrixScopes {
		t.Fatalf("read %d scopes, ceiling is %d", mx.Scopes, maxMatrixScopes)
	}
}

// itoa keeps the test free of a strconv import for the one place it needs digits.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
