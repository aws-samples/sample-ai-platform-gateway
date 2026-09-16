// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package main

// Model-access matrix: the served model set for EVERY scope of an org, in one
// request.
//
// Why it exists. The console could only ask "which models does this scope get?"
// one scope at a time (GET /admin/config plus GET /admin/config?effective=1), so
// answering "where do our teams and apps differ?" cost 2×N round trips and had no
// screen at all. Worse, with only one scope on screen the operator could not see
// the parent's ceiling, and the toggles rendered every model as ON when the scope
// had no list of its own — including models the team had denied. Turning one off
// then wrote the rest as an explicit list, GRANTING what the team refused. The
// matrix exists so that ceiling is visible and unclickable.
//
// Storage shape this has to respect (see govcore.ScopeKey / DeepMerge):
//   - one DynamoDB item per scope, pk only, no sort key — so no Query can span
//     scopes, and the IAM policy grants GetItem but not Scan/BatchGetItem;
//   - the config is a single JSON attribute;
//   - DeepMerge makes LISTS REPLACE, and there is exactly one `allowed_models`
//     list per scope. That is why "inherits" is a property of the whole SCOPE and
//     never of an individual model: a scope either has a list (it decides) or has
//     none (it follows the parent). Any per-model tri-state would be a UI fiction
//     the store cannot hold.

import (
	"context"
	"sort"
	"sync"

	"github.com/aiplat/governance/internal/govcore"
)

// maxMatrixScopes bounds how many config items one matrix request will read.
// The Lambda has a 15 s timeout and the API Gateway integration cuts at 17 s, so
// an org with thousands of apps must degrade into a truncated answer rather than
// a 504 that says nothing. Reads run in parallel (matrixReadConcurrency), so this
// ceiling is about honesty, not throughput.
const maxMatrixScopes = 400

// matrixReadConcurrency caps in-flight GetItems. High enough that the three waves
// (org, teams, apps) finish in a few hundred milliseconds for a normal org; low
// enough not to spend the whole Lambda on sockets.
const matrixReadConcurrency = 16

// accessRow is one line of the matrix: a scope and what it is actually served.
type accessRow struct {
	Kind     string `json:"kind"` // "org" | "team" | "app"
	ID       string `json:"id"`
	Team     string `json:"team,omitempty"` // parent team, for an app row
	Label    string `json:"label"`          // display name, falls back to the id
	Status   string `json:"status,omitempty"`
	ScopeKey string `json:"scope_key"`

	// HasOwn is the honest answer to "does this scope decide?". False means the
	// row inherits, and the console must render it as inherited rather than as a
	// row of allowed models — that conflation is the bug this endpoint replaces.
	HasOwn bool     `json:"has_own"`
	Own    []string `json:"own"`

	// Effective is what the gateway serves for this scope; ParentEffective is the
	// ceiling the console must not let anyone exceed.
	Effective       []string `json:"effective"`
	ParentEffective []string `json:"parent_effective"`

	// OverParent lists models this scope DECLARES that its parent refuses. Always
	// empty for an inheriting row.
	//
	// These models are NOT served: the chain intersects, so the ceiling wins. They
	// are an inert leftover — a declaration written before the ceiling tightened, or
	// through the API rather than the console. Reported so an operator can remove
	// them, because a list that names a model the gateway will never serve reads as
	// access that exists.
	OverParent []string `json:"over_parent,omitempty"`
}

// allowedFrom extracts `allowed_models` from a config document.
//
// nil = the key is ABSENT (no restriction declared at this scope). A non-nil empty
// slice = declared and empty, which denies everything. Those two are NOT the same
// thing and collapsing them is how "deny all" used to mean "allow all": the gateway
// read len==0 as unrestricted, so turning every model off granted every model.
// The chain now resolves this key by intersection, which can legitimately produce
// the empty set, so the distinction has to survive all the way here.
// Both []interface{} and []string are accepted. A document straight out of
// json.Unmarshal holds the former, but a caller that has already RESOLVED the list
// and written it back into the map holds the latter — and a single type assertion
// silently returned "not declared" for that case, which reset the ceiling mid-chain
// in the ?effective=1 merge. Handling both here removes the trap rather than leaving
// it for the next caller to step in.
func allowedFrom(cfg map[string]interface{}) []string {
	var out []string
	switch raw := cfg["allowed_models"].(type) {
	case []interface{}:
		out = make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = make([]string, 0, len(raw))
		for _, s := range raw {
			if s != "" {
				out = append(out, s)
			}
		}
	default:
		return nil
	}
	sort.Strings(out)
	return out
}

// effectiveAllowed resolves what a scope is SERVED: the parent's ceiling
// intersected with this scope's own declaration.
//
// This is the governance mirror of the Core's rule (ddbconfig.intersectAllowed) —
// see govcore.IntersectAllowed for the semantics and for why the two are duplicated
// rather than shared. Before this, a child's list simply replaced the parent's, so
// an app could be served models its team denied.
func effectiveAllowed(own, parent []string) []string {
	return govcore.IntersectAllowed(parent, own)
}

// beyond returns the members of child that parent does not contain.
//
// Applied to a scope's OWN declaration against its ceiling, it answers "which models
// does this scope ask for that the level above refuses?". Under intersection those
// models are NOT served — they are an inert leftover declaration, and naming them is
// what lets an operator clean them up instead of believing they are in effect.
func beyond(child, parent []string) []string {
	if parent == nil {
		return nil // nothing above declares a ceiling, so nothing can be over it
	}
	in := make(map[string]bool, len(parent))
	for _, m := range parent {
		in[m] = true
	}
	var out []string
	for _, m := range child {
		if !in[m] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// clipToCatalog drops entries that are not routes in the org's catalog.
//
// A model with no route cannot be served whatever any list says, so carrying it in
// the matrix only produces a phantom: the column does not exist (columns come from
// the catalog), yet the row would be flagged as declaring something above its
// ceiling — a warning with nothing to click. Happens when an alias is deleted from
// Models & Routing while a scope's allowed_models still names it.
func clipToCatalog(list, catalog []string) []string {
	if list == nil {
		return nil
	}
	in := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		in[m] = true
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		if in[m] {
			out = append(out, m)
		}
	}
	return out
}

// routingKeys returns the model aliases declared in a merged config — the catalog
// every scope draws from. Sorted, so the matrix columns do not reshuffle between
// two loads of the same data.
func routingKeys(cfg map[string]interface{}) []string {
	r, ok := cfg["routing"].(map[string]interface{})
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cloneConfig is a shallow copy, enough for the merge chain: DeepMerge replaces
// whole values for lists and scalars and recurses into maps, and a child scope
// must not mutate the parent's merged result that its siblings also read.
func cloneConfig(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m)+4)
	govcore.DeepMerge(out, m)
	return out
}

// matrixScopeReader reads one config scope. It exists so the assembly below can
// be exercised without DynamoDB.
type matrixScopeReader func(ctx context.Context, key string) map[string]interface{}

// readScopesParallel resolves several scope keys concurrently, bounded by
// matrixReadConcurrency. Order of the result matches the order of keys.
func readScopesParallel(ctx context.Context, read matrixScopeReader, keys []string) []map[string]interface{} {
	out := make([]map[string]interface{}, len(keys))
	sem := make(chan struct{}, matrixReadConcurrency)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		go func(i int, k string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = read(ctx, k)
		}(i, k)
	}
	wg.Wait()
	return out
}

// accessMatrix is the whole response.
type accessMatrix struct {
	Org    string      `json:"org"`
	Models []string    `json:"models"`
	Rows   []accessRow `json:"rows"`
	// Drift is the subset of Rows with a non-empty OverParent, lifted out so the
	// console does not have to scan for it to show the warning strip.
	Drift []accessRow `json:"drift"`
	// Truncated says the org has more scopes than maxMatrixScopes and the matrix
	// is therefore incomplete. The console must say so instead of presenting a
	// partial answer as the full picture.
	Truncated bool `json:"truncated"`
	Scopes    int  `json:"scopes_read"`
}

// buildAccessMatrix assembles the matrix. Reads are injected, so this is the part
// under test; the handler supplies readScope.
//
// Three waves rather than one read per row's full chain: an app's ceiling is its
// team's effective config, so reading a chain per row would fetch `global` and
// `ORG#` once per app. Wave 1 resolves the org, wave 2 the teams on top of it,
// wave 3 the apps on top of their own team. Total reads: 2 + teams + apps.
func buildAccessMatrix(ctx context.Context, read matrixScopeReader, org string, tree govcore.OrgTree,
	teamFilter string) accessMatrix {

	m := accessMatrix{Org: org, Rows: []accessRow{}, Drift: []accessRow{}}

	// ---- wave 1: global + the org itself ----
	//
	// Built by hand instead of with govcore.ScopeKeys(org,"",""), which returns
	// THREE keys: it forces team="default" and always appends
	// ORG#<org>#TEAM#default. That chain is correct for a REQUEST that carries no
	// team (the gateway resolves such a request under TEAM#default, which is why
	// the Core does the same) — but it is NOT the ancestry of the org scope.
	// TEAM#default is a SIBLING of the other teams, not an ancestor of the org.
	//
	// Reading the last element of that chain as "the org's own config" is a bug
	// that already shipped once: the org's allowed_models became invisible
	// (has_own always false), and orgEff therefore fell back to the whole catalog,
	// so EVERY team was shown a ceiling more permissive than the org's real one —
	// the one guarantee this screen exists to make. The write target is
	// ScopeKey (singular), and that is what has to be read back.
	orgKeys := []string{"global", govcore.ScopeKey(org, "", "")}
	orgDocs := readScopesParallel(ctx, read, orgKeys)
	m.Scopes += len(orgKeys)

	orgMerged := map[string]interface{}{}
	for _, d := range orgDocs {
		if d != nil {
			govcore.DeepMerge(orgMerged, d)
		}
	}
	// The catalog is the org's effective routing. A team or app can only ever
	// restrict within it — it cannot introduce a model of its own, which is why
	// this is also the org row's ceiling.
	m.Models = routingKeys(orgMerged)

	// orgDocs[1] is ORG#<org> — the scope the console writes to. See the comment on
	// orgKeys for why this is not the tail of govcore.ScopeKeys.
	//
	// Clipped to the catalog: a stale alias left in the org's list names a model with
	// no route, which cannot be served and has no column. Clipping here is enough for
	// the whole matrix, because every scope below intersects against this.
	orgOwnRaw := allowedFrom(orgDocs[1])
	orgOwn := clipToCatalog(orgOwnRaw, m.Models)
	orgEff := effectiveAllowed(orgOwn, m.Models)
	m.Rows = append(m.Rows, accessRow{
		Kind: "org", ID: org, Label: org, ScopeKey: govcore.ScopeKey(org, "", ""),
		HasOwn: orgOwn != nil, Own: orgOwn,
		Effective: orgEff, ParentEffective: m.Models,
		// The org's ceiling IS the catalog, so after clipping nothing can be over it.
		OverParent: beyond(orgOwn, m.Models),
	})

	// ---- which teams to show ----
	// The registry is the source of truth for existence, but two teams can be
	// reachable without appearing there: `default` (implicit — the merge chain
	// always passes through TEAM#default when no team is given) and any team an
	// app points at. Leaving those out would hide a scope that really is applied.
	teamSet := map[string]bool{}
	for id := range tree.Teams {
		teamSet[id] = true
	}
	for _, a := range tree.Apps {
		t := a.Team
		if t == "" {
			t = "default"
		}
		teamSet[t] = true
	}
	teamSet["default"] = true // read it; the row is dropped below if it is inert

	teamIDs := make([]string, 0, len(teamSet))
	for id := range teamSet {
		if teamFilter != "" && id != teamFilter {
			continue
		}
		teamIDs = append(teamIDs, id)
	}
	sort.Strings(teamIDs)

	// ---- which apps to show ----
	type appEntry struct {
		id   string
		meta govcore.AppMeta
		team string
	}
	var apps []appEntry
	for id, meta := range tree.Apps {
		t := meta.Team
		if t == "" {
			t = "default"
		}
		if teamFilter != "" && t != teamFilter {
			continue
		}
		apps = append(apps, appEntry{id: id, meta: meta, team: t})
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].team != apps[j].team {
			return apps[i].team < apps[j].team
		}
		return apps[i].id < apps[j].id
	})

	// Budget check BEFORE reading: an org past the ceiling gets a truncated
	// answer it is told about, never a timeout.
	if m.Scopes+len(teamIDs)+len(apps) > maxMatrixScopes {
		m.Truncated = true
		room := maxMatrixScopes - m.Scopes
		if room < 0 {
			room = 0
		}
		if len(teamIDs) > room {
			teamIDs = teamIDs[:room]
			apps = nil
		} else if len(teamIDs)+len(apps) > room {
			apps = apps[:room-len(teamIDs)]
		}
	}

	// ---- wave 2: teams ----
	teamKeys := make([]string, len(teamIDs))
	for i, id := range teamIDs {
		teamKeys[i] = govcore.ScopeKey(org, id, "")
	}
	teamDocs := readScopesParallel(ctx, read, teamKeys)
	m.Scopes += len(teamKeys)

	appsByTeam := map[string][]appEntry{}
	for _, a := range apps {
		appsByTeam[a.team] = append(appsByTeam[a.team], a)
	}

	teamEff := map[string][]string{}                  // ceiling for that team's apps
	teamMerged := map[string]map[string]interface{}{} // base for that team's apps
	teamRow := map[string]accessRow{}
	for i, id := range teamIDs {
		merged := cloneConfig(orgMerged)
		if teamDocs[i] != nil {
			govcore.DeepMerge(merged, teamDocs[i])
		}
		own := clipToCatalog(allowedFrom(teamDocs[i]), m.Models)
		eff := effectiveAllowed(own, orgEff)
		teamEff[id] = eff
		teamMerged[id] = merged

		meta, inRegistry := tree.Teams[id]
		label := meta.DisplayName
		if label == "" {
			label = id
		}
		// Drop a `default` row that exists for nobody: not registered, no policy
		// of its own, no apps under it. Showing it would invite configuring a
		// scope the org does not use.
		if id == "default" && !inRegistry && own == nil && len(appsByTeam[id]) == 0 {
			continue
		}
		teamRow[id] = accessRow{
			Kind: "team", ID: id, Label: label, Status: meta.Status,
			ScopeKey: teamKeys[i],
			HasOwn:   own != nil, Own: own,
			Effective: eff, ParentEffective: orgEff,
			// Against OWN, not Effective: after intersection Effective can never
			// exceed the ceiling, so comparing it would always be empty and the
			// leftover declaration would go unreported.
			OverParent: beyond(own, orgEff),
		}
	}

	// ---- wave 3: apps ----
	appKeys := make([]string, len(apps))
	for i, a := range apps {
		appKeys[i] = govcore.ScopeKey(org, a.team, a.id)
	}
	appDocs := readScopesParallel(ctx, read, appKeys)
	m.Scopes += len(appKeys)

	appRows := map[string][]accessRow{}
	for i, a := range apps {
		own := clipToCatalog(allowedFrom(appDocs[i]), m.Models)
		ceiling, ok := teamEff[a.team]
		if !ok {
			ceiling = orgEff
		}
		eff := effectiveAllowed(own, ceiling)
		label := a.meta.DisplayName
		if label == "" {
			label = a.id
		}
		appRows[a.team] = append(appRows[a.team], accessRow{
			Kind: "app", ID: a.id, Team: a.team, Label: label, Status: a.meta.Status,
			ScopeKey: appKeys[i],
			HasOwn:   own != nil, Own: own,
			Effective: eff, ParentEffective: ceiling,
			OverParent: beyond(own, ceiling),
		})
	}

	// ---- emit in hierarchy order: team, then its apps ----
	for _, id := range teamIDs {
		row, shown := teamRow[id]
		if !shown {
			continue
		}
		m.Rows = append(m.Rows, row)
		m.Rows = append(m.Rows, appRows[id]...)
	}

	for _, r := range m.Rows {
		if len(r.OverParent) > 0 {
			m.Drift = append(m.Drift, r)
		}
	}
	return m
}
