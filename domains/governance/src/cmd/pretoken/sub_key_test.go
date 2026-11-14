// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// GOV-SEC-003 behavioural test for the pre-token trigger.
//
// Proves the team scope is resolved from the member's IMMUTABLE Cognito sub, not
// the mutable e-mail. The fake table holds the MEMBER# record ONLY under the sub
// key. The event carries that sub together with a DIFFERENT e-mail (as happens
// after a member changes and re-verifies their address).
//
// On the FIXED code the trigger keys by sub, finds the record and injects the
// `team` claim. On the OLD (e-mail-keyed) code it keys by the new e-mail, misses
// the record and injects NOTHING — the member silently sheds their team scope.
// So this test FAILS behaviourally against the old code (a missing claim), it
// does not merely fail to compile.
package main

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDDB struct{ items map[string]string } // pk -> config JSON

func (f fakeDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	pkAV, _ := in.Key["pk"].(*ddbtypes.AttributeValueMemberS)
	cfg, ok := f.items[pkAV.Value]
	if !ok {
		return &dynamodb.GetItemOutput{}, nil // no item
	}
	return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
		"pk":     &ddbtypes.AttributeValueMemberS{Value: pkAV.Value},
		"config": &ddbtypes.AttributeValueMemberS{Value: cfg},
	}}, nil
}

func TestPretoken_KeysBySub_NotEmail(t *testing.T) {
	const org, sub = "acme", "9f1c2d3e-sub-immutable"
	// The record exists ONLY under the sub key.
	ddb = fakeDDB{items: map[string]string{
		"MEMBER#" + org + "#" + sub: `{"team":"sre","apps":["api"]}`,
	}}
	configTable = "cfg"
	t.Cleanup(func() { ddb = nil })

	ev := events.CognitoEventUserPoolsPreTokenGen{}
	ev.Request.UserAttributes = map[string]string{
		"custom:org_id": org,
		"sub":           sub,
		// A NEW address, deliberately different from whatever the record was first
		// created under. An e-mail-keyed lookup would miss on this.
		"email": "renamed@example.com",
	}

	out, err := handle(context.Background(), ev)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	add := out.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride
	if add["team"] != "sre" {
		t.Fatalf("GOV-SEC-003: team claim must be resolved by sub; got %q (the member escaped their scope)", add["team"])
	}
	if add["apps"] != "api" {
		t.Fatalf("GOV-SEC-003: apps claim expected \"api\", got %q", add["apps"])
	}
}

// A member with no sub (malformed event) gets NO scope — and crucially no e-mail
// fallback that would reopen the escape.
func TestPretoken_NoSub_NoClaim(t *testing.T) {
	ddb = fakeDDB{items: map[string]string{
		"MEMBER#acme#renamed@example.com": `{"team":"sre"}`,
	}}
	configTable = "cfg"
	t.Cleanup(func() { ddb = nil })

	ev := events.CognitoEventUserPoolsPreTokenGen{}
	ev.Request.UserAttributes = map[string]string{
		"custom:org_id": "acme",
		"email":         "renamed@example.com", // present, but sub is absent
	}
	out, err := handle(context.Background(), ev)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(out.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride) != 0 {
		t.Fatalf("GOV-SEC-003: no sub must mean no scope (no e-mail fallback)")
	}
}

// GOV-SEC-011: a non-admin role (dev/billing) with NO MEMBER# record must be scope-denied,
// not silently org-wide. Owners/admins with no record stay org-wide (intentional). Fails on
// the old code, which returned the event untouched for everyone with no record.
func TestPretoken_NonAdminNoRecord_ScopeDenied(t *testing.T) {
	const org, sub = "acme", "no-record-sub"
	ddb = fakeDDB{items: map[string]string{}} // no MEMBER# record at all
	configTable = "cfg"
	t.Cleanup(func() { ddb = nil })

	mk := func(role string) events.CognitoEventUserPoolsPreTokenGen {
		ev := events.CognitoEventUserPoolsPreTokenGen{}
		ev.Request.UserAttributes = map[string]string{"custom:org_id": org, "sub": sub, "custom:role": role}
		return ev
	}

	// dev with no record → scope_denied, empty team.
	out, err := handle(context.Background(), mk("dev"))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	add := out.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride
	if add["scope_denied"] != "true" {
		t.Fatalf("GOV-SEC-011: a dev with no MEMBER# record must be scope_denied; got %v", add)
	}
	if add["team"] != "" {
		t.Fatalf("GOV-SEC-011: scope-denied token must carry an empty team; got %q", add["team"])
	}

	// owner with no record → org-wide (no override), unchanged.
	outOwner, _ := handle(context.Background(), mk("owner"))
	if len(outOwner.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride) != 0 {
		t.Fatalf("an owner with no record must stay org-wide (no claim override); got %v", outOwner.Response.ClaimsOverrideDetails.ClaimsToAddOrOverride)
	}
}
