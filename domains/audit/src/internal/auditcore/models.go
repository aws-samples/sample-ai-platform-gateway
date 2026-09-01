// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package auditcore

import (
	"sort"
	"strings"
)

// ModelAction is a model action derived from the config diff, together with the model
// it affects.
type ModelAction struct {
	Action string
	Model  string
	// Changes are that model's changes, so the specific record carries only what
	// concerns it instead of the whole config diff.
	Changes []Change
}

// DeriveModelActions translates changes in the "routing" key into model actions.
//
// Why it exists: the backend has no "add model" route. The Console writes models
// INSIDE the "routing" key via PUT /admin/config. Recording that as just a generic
// config_update would be useless in exactly the use case that motivated the audit
// ("who inserted that model?"). So the specific action is DERIVED from the diff.
//
// Accepted consequence: one request may generate several events (touching three models
// = three records). That is correct — the auditor wants to see "added model X", not
// "saved the config".
//
// Classification rules, per affected model:
//   - only created fields (no Before)        → model_add
//   - only removed fields (no After)         → model_remove
//   - any other combination                  → model_update
//
// PURE: it queries nothing and knows neither instant nor identifier.
func DeriveModelActions(changes []Change) []ModelAction {
	byModel := map[string][]Change{}
	for _, c := range changes {
		model, ok := modelFromPath(c.Path)
		if !ok {
			continue
		}
		byModel[model] = append(byModel[model], c)
	}

	// Deterministic order by model name — the same reason as in Diff: without it, the
	// sequence of recorded events would vary between identical runs.
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	out := make([]ModelAction, 0, len(models))
	for _, m := range models {
		chs := byModel[m]
		out = append(out, ModelAction{Action: classify(chs), Model: m, Changes: chs})
	}
	return out
}

// classify decides the action by looking at whether that model's changes are all
// creations, all removals, or mixed.
func classify(chs []Change) string {
	allCreated, allRemoved := true, true
	for _, c := range chs {
		if c.Before != nil {
			allCreated = false
		}
		if c.After != nil {
			allRemoved = false
		}
	}
	switch {
	case allCreated && !allRemoved:
		return ActionModelAdd
	case allRemoved && !allCreated:
		return ActionModelRemove
	default:
		return ActionModelUpdate
	}
}

// modelFromPath extracts the model name from a diff path under "routing".
// Typical path: "routing.openrouter-gpt-oss-20b.provider" → the model is the second
// segment.
//
// A model name may contain a dot (e.g. "gpt-4.1"), which would break a naive split —
// so the second segment is not trustworthy on its own. But the diff flattening uses the
// dot as a separator, and there is no way to disambiguate afterwards. The choice here is
// pragmatic and recorded: take everything between "routing." and the LAST separator when
// the path has 3+ segments, treating the rest as the name.
func modelFromPath(path string) (string, bool) {
	const prefix = "routing."
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "", false
	}
	// Known fields of a model route. The model name is everything that comes BEFORE the
	// first known field — so "gpt-4.1.provider" resolves to "gpt-4.1" instead of "gpt".
	for _, field := range routeFields {
		if i := strings.Index(rest, "."+field); i > 0 {
			return rest[:i], true
		}
	}
	// A path with no known field: the whole model changed (route created/removed as a
	// single value), so the rest is the name.
	return rest, true
}

// routeFields are the top-level fields of a model route in the config.
// They serve as anchors to separate the model NAME (which may contain a dot) from the
// field. The order matters: "api_key_secret" must be tested BEFORE "api_key", otherwise
// the shorter prefix matches first and the model name comes out truncated.
var routeFields = []string{
	"provider_model_id", "provider", "base_url", "api_key_secret", "api_key",
	"capabilities", "fallback", "region", "role_arn", "external_id",
	"kind", "enabled", "prefix_cache", "headers", "timeout_ms",
}
