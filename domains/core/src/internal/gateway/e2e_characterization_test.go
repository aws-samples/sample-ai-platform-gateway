// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// E2E characterization of handle() WITHOUT AWS (hexagonal-refactor, tasks 1.1–1.3).
//
// These tests drive the Core's decision function (handle) end to end using the
// injection seams — callProviderFn (fake provider), authResolveFn (fake auth) and
// emitUsageFn (in-memory usage collector) — with every infrastructure global
// neutralized (empty tables/queue). Config comes from envConfig() (an empty
// CONFIG_TABLE ⇒ fallback), and the cache is skipped (empty CACHE_TABLE).
//
// What they capture as a GOLDEN (files in testdata/):
//   - the emitted Usage_Record, and
//   - the HTTP response (status, headers, body)
//
// across the three scenarios of the design's Error Handling table:
//   - served successfully,
//   - no eligible model (400 no_eligible_model),
//   - all providers fail (502).
//
// These goldens are the SAFETY NET for the refactor's following steps (extracting the
// provider and state adapters): if any move changes the wire serialization, the cost,
// the savings or the response format, the golden changes and the test catches it.
// Regenerate: `go test ./cmd/router -run E2E -update`.
package gateway

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/aiplat/core/internal/adapters/ddbcache"
	"github.com/aiplat/core/internal/adapters/ddbconfig"
	"github.com/aiplat/core/internal/adapters/ddbkeys"
	"github.com/aiplat/core/internal/adapters/ddblimits"
	"github.com/aiplat/core/internal/adapters/secrets"
	"github.com/aiplat/core/internal/adapters/sqsusage"
	"github.com/aiplat/core/internal/httpapi"
	"github.com/aiplat/core/internal/ports"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/")

// capturedGolden is what we serialize to disk: HTTP response + usage records.
type capturedGolden struct {
	Status  int                      `json:"status"`
	Headers map[string]string        `json:"headers"`
	Body    interface{}              `json:"body"`
	Records []map[string]interface{} `json:"records"`
}

// neutralizeCoreGlobals blanks the infrastructure globals so handle can run offline.
// They are all "" by default in the test process; reasserting them here makes the test
// robust against execution order and against whoever runs it with env vars set.
func neutralizeCoreGlobals(t *testing.T) {
	t.Helper()
	// Adapters with empty tables/queue and nil clients: config falls back to the
	// environment defaults, cache/limits/usage/keys become no-ops. Rebuilding them here
	// also clears the config cache (isolation between scenarios).
	configStore = ddbconfig.New(nil, "", "")
	cacheStore = ddbcache.New(nil, "")
	limitsStore = ddblimits.New(nil, "")
	usageSink = sqsusage.New(nil, "")
	secretStore = secrets.New(nil)
	keyStore = ddbkeys.New(nil, "")
	hintsReader = nil
}

// fakeAuth replaces authResolveFn: it resolves a fixed team/app without touching DDB.
func fakeAuth(_ context.Context, _ map[string]string) (identity, bool, error) {
	return identity{team: "default", app: "apptest"}, true, nil
}

// installSeams installs the fake provider, fake auth and usage collector, restoring the
// defaults at the end of the test. It returns the pointer to the collected records.
func installSeams(t *testing.T, provider func(context.Context, Route, []chatMsg, []toolDef) (result, error)) *[]map[string]interface{} {
	t.Helper()
	prevAuth, prevProv, prevEmit := authResolveFn, callProviderFn, emitUsageFn
	recs := &[]map[string]interface{}{}
	authResolveFn = fakeAuth
	callProviderFn = provider
	emitUsageFn = func(_ context.Context, rec map[string]interface{}) {
		// Defensive copy: handle reuses and mutates nested maps after emitting.
		*recs = append(*recs, deepCopyMap(rec))
	}
	t.Cleanup(func() {
		authResolveFn, callProviderFn, emitUsageFn = prevAuth, prevProv, prevEmit
	})
	return recs
}

// --- normalization of volatile fields ----------------------------------------

// volatileKeys are the fields that change on every run and cannot enter the golden.
var volatileKeys = map[string]bool{
	"latency_ms": true,
	"ts":         true,
}

// normalize recursively replaces the volatile fields with stable sentinels, so the
// golden compares only behavior (not the clock nor the stopwatch).
func normalize(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			if volatileKeys[k] {
				out[k] = "<volatile>"
				continue
			}
			out[k] = normalize(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	default:
		return v
	}
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	b, _ := json.Marshal(m)
	var out map[string]interface{}
	json.Unmarshal(b, &out)
	return out
}

// roundTripJSON converts any value into the canonical JSON form (map/[]/prim), making
// sure the comparison with the golden is type by type (no int vs float64).
func roundTripJSON(v interface{}) interface{} {
	b, _ := json.Marshal(v)
	var out interface{}
	json.Unmarshal(b, &out)
	return out
}

// runScenario drives handle, collects the response + records, normalizes them and
// compares against (or rewrites) the golden.
func runScenario(t *testing.T, name string, req httpapi.Request, recs *[]map[string]interface{}, resp httpapi.Response) {
	t.Helper()

	// Body: if it is JSON, keep it as a structure (readable diff); otherwise, raw string.
	var body interface{}
	if json.Unmarshal([]byte(resp.Body), &body) != nil {
		body = resp.Body
	}

	normRecs := make([]map[string]interface{}, 0, len(*recs))
	for _, r := range *recs {
		normRecs = append(normRecs, normalize(roundTripJSON(r)).(map[string]interface{}))
	}

	got := capturedGolden{
		Status:  resp.StatusCode,
		Headers: resp.Headers,
		Body:    normalize(body),
		Records: normRecs,
	}

	path := filepath.Join("testdata", name+".golden.json")
	gotJSON, _ := json.MarshalIndent(got, "", "  ")

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, append(gotJSON, '\n'), 0o644); err != nil {
			t.Fatalf("writing golden %s: %v", path, err)
		}
		t.Logf("golden rewritten: %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden missing (%s): run with -update. error: %v", path, err)
	}
	// Compare by canonical form (re-serialize the golden that was read).
	var wantAny, gotAny interface{}
	json.Unmarshal(want, &wantAny)
	json.Unmarshal(gotJSON, &gotAny)
	if !jsonEqual(wantAny, gotAny) {
		t.Errorf("scenario %q diverged from golden %s.\n--- expected ---\n%s\n--- got ---\n%s",
			name, path, string(want), string(gotJSON))
	}
}

func jsonEqual(a, b interface{}) bool {
	ba, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ba) == string(bb)
}

// --- scenarios ---------------------------------------------------------------

// Scenario 1: served successfully. One eligible model, the fake provider answers OK.
func TestE2E_SucessoServido(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		return result{text: "olá do provedor falso", tin: 10, tout: 5, cacheConv: ports.CacheCountersAbsent}, nil
	})

	req := httpapi.Request{
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"oi"}]}`,
		RequestID: "test-req-sucesso",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, expected 200", resp.StatusCode)
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 emitted Usage_Record, got %d", len(*recs))
	}
	runScenario(t, "sucesso_servido", req, recs, resp)
}

// Scenario 2: no eligible model → 400 no_eligible_model.
// The requested model exists in the catalog (otherwise it would be unknown_model), but
// the request carries tools and the only model does not declare tool use → eligibility
// empties out and the domain returns ErrNoEligibleModel.
func TestE2E_NenhumModeloElegivel(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":false,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	called := false
	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		called = true
		return result{}, nil
	})

	req := httpapi.Request{
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"chame a ferramenta"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
		RequestID: "test-req-inelegivel",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, expected 400", resp.StatusCode)
	}
	if called {
		t.Error("the provider should NOT be called when there is no eligible model")
	}
	runScenario(t, "nenhum_modelo_elegivel", req, recs, resp)
}

// Scenario 3: all providers fail → 502.
// An eligible model, but the fake provider returns an error on every attempt; the
// fallback loop runs out, handle emits an error Usage_Record and answers 502.
func TestE2E_TodosProvedoresFalham(t *testing.T) {
	neutralizeCoreGlobals(t)
	t.Setenv("MODEL_ROUTING", `{"m1":{"provider":"bedrock","provider_model_id":"id1","capabilities":{"tool_use":true,"tier":"fast"}}}`)
	t.Setenv("PRICING_TABLE", `{"m1":{"input":0.001,"output":0.002}}`)

	recs := installSeams(t, func(_ context.Context, _ Route, _ []chatMsg, _ []toolDef) (result, error) {
		return result{}, fmt.Errorf("provider down: connection refused")
	})

	req := httpapi.Request{
		Method:    "POST",
		Path:      "/v1/chat/completions",
		Headers:   map[string]string{"authorization": "Bearer test"},
		Body:      `{"model":"m1","messages":[{"role":"user","content":"oi"}]}`,
		RequestID: "test-req-falha",
	}
	resp, err := handle(context.Background(), req)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, expected 502", resp.StatusCode)
	}
	if len(*recs) != 1 {
		t.Fatalf("expected 1 error Usage_Record, got %d", len(*recs))
	}
	runScenario(t, "todos_provedores_falham", req, recs, resp)
}

// Guarantees a stable key order when serializing (defensive; encoding/json already
// sorts map keys, but this keeps the intent explicit).
var _ = sort.Strings
