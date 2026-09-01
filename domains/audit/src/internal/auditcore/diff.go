// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import (
	"encoding/json"
	"sort"
	"strings"
)

// Change is a change to ONE field. The path uses stable dotted notation
// ("budget.limit_usd", "routing.openrouter-gpt-oss-20b.provider") because that is what
// makes it possible to filter and read the trail later — "config changed" allows no
// investigation at all, and the precision of the diff is the reason this feature exists.
//
// A missing Before/After distinguishes creation from removal: a created field has no
// Before, a removed field has no After.
type Change struct {
	Path     string `json:"path"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// RedactedMarker replaces a sensitive value. Text (and not an empty string) so the
// reader of the trail sees that a change HAPPENED without seeing the value — "field
// absent" and "secret field changed" are different things in an audit.
const RedactedMarker = "«redigido»"

// MaxChanges is the ceiling of entries per record. It exists for a hard reason, not an
// aesthetic one: a PUT /admin/config rewrites the whole "routing" key (dozens of
// models, each with capabilities and price). Without a ceiling, a single record could
// blow past DynamoDB's 400 KB item and MAKE THE WRITE FAIL — the excess of detail
// would destroy exactly the trail one wants to keep.
const MaxChanges = 100

// Diff compares two objects and returns one entry per changed field.
//
// Guaranteed properties (verified by tests):
//   - it never emits an entry whose Before equals its After;
//   - the order is a function only of the SET of paths (sorted), never of the map's
//     iteration order — without that the test would be flaky and the stored diff would
//     change shape between two identical runs.
func Diff(before, after map[string]any) []Change {
	flatBefore := map[string]any{}
	flatAfter := map[string]any{}
	flatten("", before, flatBefore)
	flatten("", after, flatAfter)

	paths := make([]string, 0, len(flatBefore)+len(flatAfter))
	seen := map[string]bool{}
	for p := range flatBefore {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for p := range flatAfter {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	out := make([]Change, 0, len(paths))
	for _, p := range paths {
		b, hadB := flatBefore[p]
		a, hadA := flatAfter[p]
		if hadB && hadA && sameValue(b, a) {
			continue // an unchanged field is not a change
		}
		ch := Change{Path: p}
		if hadB {
			ch.Before = b
		}
		if hadA {
			ch.After = a
		}
		out = append(out, ch)
	}
	return out
}

// flatten flattens the object into dotted paths. A list is NOT flattened by index: a
// slice is compared as a single value, because an array index is an unstable path —
// inserting an item at the front would make "every index changed", filling the diff
// with noise instead of saying what actually changed.
func flatten(prefix string, v map[string]any, out map[string]any) {
	for k, val := range v {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if sub, ok := val.(map[string]any); ok && len(sub) > 0 {
			flatten(path, sub, out)
			continue
		}
		out[path] = val
	}
}

// sameValue compares by canonical form (JSON). Comparing with == would break for
// maps/slices, and the data comes from JSON, where a number is always float64 — the
// canonical form avoids a false positive change between 50 and 50.0.
func sameValue(a, b any) bool {
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	if ea != nil || eb != nil {
		return false // could not compare: treat as a change (conservative)
	}
	return string(ja) == string(jb)
}

// sensitiveLeaves are the field names whose VALUE may never be persisted.
// The comparison is on the LAST segment of the path, not on a substring of the whole
// path — the difference matters and is the trap case of this function:
//
//	"routing.openrouter.api_key"        → credential value            → REDACT
//	"routing.openrouter.api_key_secret" → NAME of the referenced secret → do NOT redact
//
// The secret's name is exactly what one wants to audit ("switched to credential X");
// redacting it would make the trail useless in the real use case.
var sensitiveLeaves = map[string]bool{
	"api_key":      true,
	"apikey":       true,
	"password":     true,
	"passwd":       true,
	"secret":       true,
	"secret_value": true,
	"token":        true,
	"access_token": true,
	"credential":   true,
	"credentials":  true,
	"private_key":  true,
}

// IsSensitivePath reports whether the value at that path is a secret.
func IsSensitivePath(path string) bool {
	leaf := path
	if i := strings.LastIndex(path, "."); i >= 0 {
		leaf = path[i+1:]
	}
	return sensitiveLeaves[strings.ToLower(leaf)]
}

// Redact swaps the previous and the new value of every sensitive field for the marker,
// preserving the fact that a change happened.
//
// This function is called TWICE in the flow: at the emitter (the right place — the
// value never even crosses the queue) and again at the writer, on ingestion. The
// redundancy is intentional: a new emitter with a bug would turn a secret into an
// append-only record, which by definition cannot be corrected later. Redacting twice is
// cheap; a secret in an immutable log is irreversible.
func Redact(changes []Change) []Change {
	out := make([]Change, len(changes))
	for i, c := range changes {
		if IsSensitivePath(c.Path) {
			if c.Before != nil {
				c.Before = RedactedMarker
			}
			if c.After != nil {
				c.After = RedactedMarker
			}
			c.Redacted = true
		}
		out[i] = c
	}
	return out
}

// Truncate caps the list, returning whether it was cut. The caller records the total
// COUNT separately, so the truncation stays visible instead of silent — a diff cut
// without notice would make the auditor conclude only those fields changed.
func Truncate(changes []Change, max int) ([]Change, bool) {
	if max <= 0 || len(changes) <= max {
		return changes, false
	}
	return changes[:max], true
}
