// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package contract models the Environment Contract (SSM) and the Apply Order.
//
// Feature: multi-account-decoupling.
//
// Contract path: /${project}/${environment}/<domain>/<key>
// (e.g.: /aiplat/poc/governance/cognito_client_id). No extra fixed segment.
package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// BuildPath builds the SSM parameter path for the Environment Contract.
func BuildPath(project, environment, dom, key string) string {
	return "/" + project + "/" + environment + "/" + dom + "/" + key
}

var pathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)/([^/]+)/(.+)$`)

// ParsePath recovers (project, environment, dom, key) from a Contract path.
func ParsePath(path string) (project, environment, dom, key string, err error) {
	m := pathRe.FindStringSubmatch(path)
	if m == nil {
		return "", "", "", "", fmt.Errorf("invalid contract path: %q", path)
	}
	return m[1], m[2], m[3], m[4], nil
}

// Resolve models coalesce(var.override, ssm) over NON-EMPTY values:
// override wins when non-nil and non-empty; otherwise the SSM value applies.
// (Requirements 4.5 — the environment override always beats the Contract.)
func Resolve(override, ssmValue *string) string {
	if override != nil && *override != "" {
		return *override
	}
	if ssmValue != nil {
		return *ssmValue
	}
	return ""
}

// --- Contract registry: key -> (producer, relative path) ---
// Used for missing-parameter error messages (Requirement 2.5).

// Param describes a parameter published in the Contract.
type Param struct {
	Producer string // producer domain (e.g.: "governance")
	Key      string // key after the root (e.g.: "governance/cognito_client_id")
}

// Params are the NON-derivable identifiers published in SSM.
var Params = []Param{
	{"governance", "governance/admin_api_url"},
	{"governance", "governance/cognito_user_pool_id"},
	{"governance", "governance/cognito_client_id"},
	{"governance", "governance/cognito_issuer"},
	{"observability", "observability/usage_api_url"},
	{"core", "core/gateway_url"},
	{"core", "core/keyadmin_url"},
	{"frontend", "frontend/landing_url"},
	{"frontend", "frontend/console_url"},
	{"backoffice", "backoffice/ops_api_url"},
	{"backoffice", "backoffice/operator_console_url"},
}

// MissingParamError returns the message for a parameter missing at apply time.
func MissingParamError(project, environment, key string) string {
	producer := "?"
	for _, p := range Params {
		if p.Key == key {
			producer = p.Producer
			break
		}
	}
	full := "/" + project + "/" + environment + "/" + key
	return fmt.Sprintf("parameter %s missing (produced by domain %s); apply %s before this stack", full, producer, producer)
}

// --- Dependency DAG and Apply Order (Requirements 6.1, 6.2) ---

// Edge is an apply-time dependency producer -> consumer.
type Edge struct{ Producer, Consumer string }

// DefaultGraph holds the apply-time edges (only the non-derivable, via SSM).
// governance owns Cognito; the gov<->obs cycle is broken because governance
// derives cost_store_table/event_bus_name by convention (it does not read obs's SSM).
var DefaultGraph = []Edge{
	{"governance", "observability"},
	{"governance", "core"},
	{"governance", "frontend"},
	{"governance", "backoffice"},
	{"core", "frontend"},
	{"observability", "frontend"},
}

// TopoOrder returns a domain->position map (1..N) that is a topological
// ordering: for every producer->consumer edge, pos(producer) < pos(consumer),
// and the positions are a permutation of 1..N (unique position). Stable tie-break
// by name for determinism. Errors if there is a cycle.
func TopoOrder(nodes []string, edges []Edge) (map[string]int, error) {
	indeg := map[string]int{}
	adj := map[string][]string{}
	present := map[string]bool{}
	for _, n := range nodes {
		present[n] = true
		indeg[n] = 0
	}
	for _, e := range edges {
		if !present[e.Producer] || !present[e.Consumer] {
			return nil, fmt.Errorf("edge with unknown node: %s->%s", e.Producer, e.Consumer)
		}
		adj[e.Producer] = append(adj[e.Producer], e.Consumer)
		indeg[e.Consumer]++
	}
	pos := map[string]int{}
	next := 1
	for len(pos) < len(nodes) {
		// candidates: indegree 0 and not yet positioned, in stable order.
		var ready []string
		for _, n := range nodes {
			if _, done := pos[n]; !done && indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("cycle detected in the dependency graph")
		}
		sort.Strings(ready)
		n := ready[0]
		pos[n] = next
		next++
		for _, c := range adj[n] {
			indeg[c]--
		}
	}
	return pos, nil
}

// ApplyOrder returns the domains ordered by topological position.
func ApplyOrder(nodes []string, edges []Edge) ([]string, error) {
	pos, err := TopoOrder(nodes, edges)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(nodes))
	copy(out, nodes)
	sort.Slice(out, func(i, j int) bool { return pos[out[i]] < pos[out[j]] })
	return out, nil
}

// Domains are the platform's 5 domains.
var Domains = []string{"governance", "observability", "core", "frontend", "backoffice"}

// NormalizeKey removes the root from a full path, returning the relative key.
func NormalizeKey(path string) string {
	p, e, dom, key, err := ParsePath(path)
	if err != nil {
		return ""
	}
	_ = p
	_ = e
	return dom + "/" + key
}

// trim helper (avoids an import just for this in the package's users)
func trimRoot(path, project, environment string) string {
	return strings.TrimPrefix(path, "/"+project+"/"+environment+"/")
}

var _ = trimRoot
