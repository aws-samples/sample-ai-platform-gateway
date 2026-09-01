// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package context

import (
	"context"
	"sync"
	"testing"
)

func TestInitOrg(t *testing.T) {
	// Reset state for test isolation
	once = sync.Once{}
	singleOrg = nil
	orgInitialized = false

	InitOrg("TestOrg", "testorg.com", "admin@testorg.com")

	org := GetOrg()
	if org == nil {
		t.Fatal("GetOrg() returned nil after InitOrg")
	}
	if org.OrgName != "TestOrg" {
		t.Errorf("expected OrgName=TestOrg, got %s", org.OrgName)
	}
	if org.OrgDomain != "testorg.com" {
		t.Errorf("expected OrgDomain=testorg.com, got %s", org.OrgDomain)
	}
	if org.AdminEmail != "admin@testorg.com" {
		t.Errorf("expected AdminEmail=admin@testorg.com, got %s", org.AdminEmail)
	}
}

func TestInitOrgIdempotent(t *testing.T) {
	// Reset state for test isolation
	once = sync.Once{}
	singleOrg = nil
	orgInitialized = false

	InitOrg("First", "first.com", "admin@first.com")
	InitOrg("Second", "second.com", "admin@second.com")

	org := GetOrg()
	if org.OrgName != "First" {
		t.Errorf("expected first InitOrg to win, got %s", org.OrgName)
	}
}

func TestOrgName(t *testing.T) {
	// Reset state for test isolation
	once = sync.Once{}
	singleOrg = nil
	orgInitialized = false

	// Before initialization
	if name := OrgName(); name != "default" {
		t.Errorf("expected default before init, got %s", name)
	}

	// After initialization
	InitOrg("MyOrg", "myorg.com", "admin@myorg.com")
	if name := OrgName(); name != "MyOrg" {
		t.Errorf("expected MyOrg after init, got %s", name)
	}
}

func TestRequestContext(t *testing.T) {
	ctx := context.Background()

	reqCtx := &RequestContext{
		Team:    "backend",
		App:     "api-prod",
		Feature: "chat",
	}

	ctx = WithRequestContext(ctx, reqCtx)

	retrieved := GetRequestContext(ctx)
	if retrieved == nil {
		t.Fatal("GetRequestContext returned nil")
	}
	if retrieved.Team != "backend" {
		t.Errorf("expected Team=backend, got %s", retrieved.Team)
	}
	if retrieved.App != "api-prod" {
		t.Errorf("expected App=api-prod, got %s", retrieved.App)
	}
	if retrieved.Feature != "chat" {
		t.Errorf("expected Feature=chat, got %s", retrieved.Feature)
	}
}

func TestRequestContextHelpers(t *testing.T) {
	ctx := context.Background()

	// Without context
	if team := TeamFromContext(ctx); team != "default" {
		t.Errorf("expected default team, got %s", team)
	}
	if app := AppFromContext(ctx); app != "default" {
		t.Errorf("expected default app, got %s", app)
	}
	if feature := FeatureFromContext(ctx); feature != "" {
		t.Errorf("expected empty feature, got %s", feature)
	}

	// With context
	reqCtx := &RequestContext{
		Team:    "frontend",
		App:     "web-app",
		Feature: "analytics",
	}
	ctx = WithRequestContext(ctx, reqCtx)

	if team := TeamFromContext(ctx); team != "frontend" {
		t.Errorf("expected frontend team, got %s", team)
	}
	if app := AppFromContext(ctx); app != "web-app" {
		t.Errorf("expected web-app, got %s", app)
	}
	if feature := FeatureFromContext(ctx); feature != "analytics" {
		t.Errorf("expected analytics feature, got %s", feature)
	}
}

func TestGetRequestContextNil(t *testing.T) {
	ctx := context.Background()

	if rc := GetRequestContext(ctx); rc != nil {
		t.Error("expected nil from context without RequestContext")
	}
}
