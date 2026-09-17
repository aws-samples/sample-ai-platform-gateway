// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package bedrockgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiplat/core/internal/ports"
)

// recordingSigner captures what it was asked to sign, so the tests can assert the service
// and region a real request would be signed with WITHOUT needing AWS credentials.
type recordingSigner struct {
	calls   int
	service string
	region  string
	payload []byte
	err     error
}

func (s *recordingSigner) SignRequest(_ context.Context, req *http.Request, payload []byte, service, region string) error {
	s.calls++
	s.service, s.region = service, region
	s.payload = payload
	if s.err != nil {
		return s.err
	}
	req.Header.Set("authorization", "AWS4-HMAC-SHA256 Credential=test/...")
	return nil
}

// serve stands up a fake gateway, builds the provider against it and returns the request
// the provider actually made.
func serve(t *testing.T, r Route, signer Signer, canned string) (*http.Request, []byte, ports.Result, error) {
	t.Helper()
	var gotReq *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotReq = req.Clone(context.Background())
		gotBody, _ = io.ReadAll(req.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, canned)
	}))
	t.Cleanup(srv.Close)
	if r.BaseURL == "" {
		r.BaseURL = srv.URL
	}
	p, err := New(srv.Client(), signer, r)
	if err != nil {
		return nil, nil, ports.Result{}, err
	}
	res, err := p.Invoke(context.Background(), ports.InvokeInput{
		Messages: []ports.Message{{Role: "user", Text: "hi"}},
	})
	return gotReq, gotBody, res, err
}

const okResponse = `{"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn",
	"usage":{"input_tokens":13,"output_tokens":4,"cache_creation_input_tokens":17515,"cache_read_input_tokens":0}}`

func TestNew_RequiresSigner(t *testing.T) {
	// An unsigned request is rejected by the real gateway with a 403 that reads like bad
	// credentials. Failing here names the actual cause.
	_, err := New(http.DefaultClient, nil, Route{BaseURL: "https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com"})
	if err == nil {
		t.Fatal("expected an error when no signer is configured, got nil")
	}
	if !strings.Contains(err.Error(), "signer") {
		t.Errorf("error should name the missing signer, got %q", err)
	}
}

func TestNew_RequiresBaseURL(t *testing.T) {
	_, err := New(http.DefaultClient, &recordingSigner{}, Route{Region: "us-east-1"})
	if err == nil {
		t.Fatal("expected an error when the route has no base_url, got nil")
	}
}

func TestNew_RegionFromHostOrRoute(t *testing.T) {
	cases := []struct {
		name    string
		route   Route
		want    string
		wantErr bool
	}{
		{
			name:  "derived from the gateway host",
			route: Route{BaseURL: "https://mygw-abc123.gateway.bedrock-agentcore.eu-west-1.amazonaws.com"},
			want:  "eu-west-1",
		},
		{
			// An explicit region must win: it is the only way to sign correctly when the
			// endpoint is reached through something that rewrites the host.
			name:  "route region wins over the host",
			route: Route{BaseURL: "https://mygw.gateway.bedrock-agentcore.eu-west-1.amazonaws.com", Region: "us-east-1"},
			want:  "us-east-1",
		},
		{
			// A host with no region in it and no region on the route cannot be signed. It
			// must fail loudly rather than sign with an arbitrary default, because a
			// wrong-region signature is rejected as if the credentials were bad.
			name:    "unknown host without a route region is an error",
			route:   Route{BaseURL: "https://example.invalid"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sg := &recordingSigner{}
			r := tc.route
			// Point at a local server so Invoke does not leave the machine; the region
			// under test still comes from the configured BaseURL/Region.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, okResponse)
			}))
			defer srv.Close()

			p, err := New(srv.Client(), sg, r)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Redirect the adapter at the test server without changing the region we
			// resolved, by invoking through a client whose transport rewrites the host.
			hc := &http.Client{Transport: rewriteHost{to: srv.Listener.Addr().String()}}
			p2, err := New(hc, sg, r)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_ = p
			if _, err := p2.Invoke(context.Background(), ports.InvokeInput{
				Messages: []ports.Message{{Role: "user", Text: "hi"}},
			}); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if sg.region != tc.want {
				t.Errorf("signing region = %q, want %q", sg.region, tc.want)
			}
			if sg.service != SigningService {
				t.Errorf("signing service = %q, want %q", sg.service, SigningService)
			}
		})
	}
}

// rewriteHost sends every request to a fixed address over plain HTTP, keeping the signed
// URL intact. It is how the region tests exercise a real gateway hostname offline.
type rewriteHost struct{ to string }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = r.to
	return http.DefaultTransport.RoundTrip(clone)
}

func TestInvoke_PathAuthAndBodyVersion(t *testing.T) {
	sg := &recordingSigner{}
	req, body, res, err := serve(t, Route{Region: "us-east-1", ModelID: "us.anthropic.claude-opus-4-8"}, sg, okResponse)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// The /inference prefix is what selects an inference target. Without it the gateway
	// answers "No Target found for Target name: v1", which is a 404 and not a model error.
	if req.URL.Path != MessagesPath {
		t.Errorf("path = %q, want %q", req.URL.Path, MessagesPath)
	}

	// anthropic_version must be in the BODY. The gateway rejects a request without it
	// ("anthropic_version: Field required") — a failure that only shows up against the
	// live service, so it is asserted here.
	var sent map[string]interface{}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if sent["anthropic_version"] != BodyVersion {
		t.Errorf("anthropic_version = %v, want %q", sent["anthropic_version"], BodyVersion)
	}
	if sent["model"] != "us.anthropic.claude-opus-4-8" {
		t.Errorf("model = %v", sent["model"])
	}

	// Auth is a signature, never a key: this host has no API key to send, and setting the
	// header anyway would leak an empty credential into the request.
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key must not be set on a signed request, got %q", got)
	}
	// The version header belongs to Anthropic's own API and is mutually exclusive with the
	// body field.
	if got := req.Header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version header must not be set when the body carries it, got %q", got)
	}
	if sg.calls != 1 {
		t.Errorf("signer called %d times, want 1", sg.calls)
	}
	if string(sg.payload) != string(body) {
		t.Error("the signer must hash the EXACT body sent; a mismatch produces a 403")
	}

	// Cache counters must survive: they are what keeps the savings ledger honest on this
	// route, and the chat-completions path of the same gateway drops them entirely.
	if res.CacheWriteInputTokens != 17515 || res.InputTokens != 13 {
		t.Errorf("usage not carried through: %+v", res)
	}
	if res.CacheCounters != ports.CacheCountersReported {
		t.Errorf("CacheCounters = %q, want %q", res.CacheCounters, ports.CacheCountersReported)
	}
}

func TestInvoke_PromptCacheMarksSystemBlock(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotBody, _ = io.ReadAll(req.Body)
		io.WriteString(w, okResponse)
	}))
	t.Cleanup(srv.Close)

	p, err := New(srv.Client(), &recordingSigner{}, Route{
		BaseURL: srv.URL, Region: "us-east-1", ModelID: "m", PromptCache: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Invoke(context.Background(), ports.InvokeInput{Messages: []ports.Message{
		{Role: "system", Text: "stable prefix"},
		{Role: "user", Text: "hi"},
	}}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(string(gotBody), `"cache_control"`) {
		t.Errorf("prompt_cache must place a cache_control block on system; body was:\n%s", gotBody)
	}
}

func TestInvoke_SignerErrorIsReturned(t *testing.T) {
	// A signing failure must abort the call. Sending the request unsigned instead would
	// turn a credentials problem into a gateway auth error against a healthy service.
	sg := &recordingSigner{err: context.DeadlineExceeded}
	_, _, _, err := serve(t, Route{Region: "us-east-1", ModelID: "m"}, sg, okResponse)
	if err == nil {
		t.Fatal("expected the signing error to surface, got nil")
	}
}

func TestInvoke_ErrorLabelNamesThisProvider(t *testing.T) {
	// "anthropic 403" on a gateway route sends whoever reads the log to the wrong service.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"nope"}`)
	}))
	t.Cleanup(srv.Close)

	p, err := New(srv.Client(), &recordingSigner{}, Route{BaseURL: srv.URL, Region: "us-east-1", ModelID: "m"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Invoke(context.Background(), ports.InvokeInput{Messages: []ports.Message{{Role: "user", Text: "hi"}}})
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "bedrock_gateway") {
		t.Errorf("error must name bedrock_gateway, got %q", err)
	}
}

func TestNativeStreamingIsOffered(t *testing.T) {
	// The gateway documents SSE pass-through on this path, so the route must be able to
	// lower time-to-first-token instead of falling back to buffered pseudo-streaming.
	p, err := New(http.DefaultClient, &recordingSigner{}, Route{
		BaseURL: "https://gw.gateway.bedrock-agentcore.us-east-1.amazonaws.com", ModelID: "m",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := p.(ports.StreamProvider); !ok {
		t.Error("bedrock_gateway provider must implement ports.StreamProvider")
	}
}
