// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package reconcile

import "testing"

// A fallback routing record (measured shape, B5) yields the served target and an attempt
// count of hop+1.
func TestParseLogLine_FallbackServed(t *testing.T) {
	line := []byte(`{"resource_arn":"arn:aws:bedrock-agentcore:us-east-1:1:gateway/g",` +
		`"event_timestamp":1793305983325,"request_id":"gw-req-1",` +
		`"body":{"id":"gw-req-1","routing":{"outcome":"QUALIFIED","matchType":"QUALIFIED_OVERRIDE",` +
		`"requestedModelId":"fallback-probe","resolvedModelId":"us.amazon.nova-micro-v1:0",` +
		`"targetName":"invoke","requestPath":"/model/fallback-probe/invoke","reason":"servedViaFallbackHop:1"}}}`)
	f, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("expected a routing fact")
	}
	if f.GatewayRequestID != "gw-req-1" || f.ResolvedModel != "us.amazon.nova-micro-v1:0" || f.TargetName != "invoke" {
		t.Fatalf("fields wrong: %+v", f)
	}
	if f.ServedHop != 1 || f.Invocations != 2 || !f.Fallback {
		t.Fatalf("hop/invocations/fallback wrong: %+v", f)
	}
	if f.RequestedModel != "fallback-probe" || f.EventTS != 1793305983325 {
		t.Fatalf("requested/ts wrong: %+v", f)
	}
}

// A primary-served record (no servedViaFallbackHop) is hop 0, one invocation, not a fallback.
func TestParseLogLine_PrimaryServed(t *testing.T) {
	line := []byte(`{"event_timestamp":1793325198064,"request_id":"gw-req-2",` +
		`"body":{"id":"gw-req-2","routing":{"outcome":"QUALIFIED","matchType":"QUALIFIED_OVERRIDE",` +
		`"requestedModelId":"us.amazon.nova-micro-v1:0","resolvedModelId":"us.amazon.nova-micro-v1:0",` +
		`"targetName":"invoke","requestPath":"/model/us.amazon.nova-micro-v1:0/invoke"},"log":"Model routing succeeded"}}`)
	f, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("expected a routing fact")
	}
	if f.ServedHop != 0 || f.Invocations != 1 || f.Fallback {
		t.Fatalf("primary serve should be hop 0 / 1 invocation / not fallback: %+v", f)
	}
}

// Non-routing lines (the gateway also logs action-resolved and http-execution lines) are
// NOT facts.
func TestParseLogLine_NonRoutingLinesIgnored(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"request_id":"x","body":{"id":"x","isError":false,"log":"Executing Http request for target TOB - RequestId: x"}}`),
		[]byte(`{"request_id":"x","body":{"id":"x","log":"Action resolved from rule r (priority 50.0): Route to target: targetName=invoke"}}`),
		[]byte(`not json at all`),
		[]byte(`{"body":{"routing":{"targetName":"invoke"}}}`), // routing present but no resolvedModelId
	}
	for i, c := range cases {
		if _, ok := ParseLogLine(c); ok {
			t.Fatalf("case %d must not be a routing fact: %s", i, c)
		}
	}
}

// parseServedHop is robust to odd reasons.
func TestParseServedHop(t *testing.T) {
	tbl := map[string]int{
		"servedViaFallbackHop:1":               1,
		"servedViaFallbackHop:2,other":         2,
		"servedViaFallbackHop:0":               0,
		"Model routing succeeded":              0,
		"":                                     0,
		"servedViaFallbackHop:":                0,
		"servedViaFallbackHop:notanumber":      0,
		"prefix servedViaFallbackHop:3 suffix": 3,
	}
	for reason, want := range tbl {
		if got := parseServedHop(reason); got != want {
			t.Errorf("parseServedHop(%q) = %d, want %d", reason, got, want)
		}
	}
}
