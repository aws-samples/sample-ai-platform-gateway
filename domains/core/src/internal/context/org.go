// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package context provides single-org deployment context for AIPlat.
// This replaces the multi-tenant SaaS model with a single organization
// per deployment, while preserving team/app hierarchy for cost tracking.
package context

import (
	"context"
	"sync"
)

// Org represents the single organization in this deployment.
type Org struct {
	OrgName    string
	OrgDomain  string
	AdminEmail string
}

// RequestContext carries the team/app/feature scope for a single request.
// This enables cost tracking and configuration hierarchy without multi-tenancy.
type RequestContext struct {
	Team    string
	App     string
	Feature string
}

var (
	once           sync.Once
	singleOrg      *Org
	orgInitialized bool
)

// InitOrg initializes the single organization for this deployment.
// This should be called once at startup. Subsequent calls are ignored.
func InitOrg(name, domain, adminEmail string) {
	once.Do(func() {
		singleOrg = &Org{
			OrgName:    name,
			OrgDomain:  domain,
			AdminEmail: adminEmail,
		}
		orgInitialized = true
	})
}

// GetOrg returns the single organization for this deployment.
// Returns nil if InitOrg has not been called.
func GetOrg() *Org {
	if !orgInitialized {
		return nil
	}
	return singleOrg
}

// OrgName returns the organization name, or "default" if not initialized.
func OrgName() string {
	if singleOrg != nil {
		return singleOrg.OrgName
	}
	return "default"
}

// contextKey is a private type for context keys to avoid collisions.
type contextKey int

const (
	requestContextKey contextKey = iota
)

// WithRequestContext adds request context to a context.Context.
func WithRequestContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey, rc)
}

// GetRequestContext retrieves the request context from a context.Context.
// Returns nil if no request context is set.
func GetRequestContext(ctx context.Context) *RequestContext {
	if rc, ok := ctx.Value(requestContextKey).(*RequestContext); ok {
		return rc
	}
	return nil
}

// TeamFromContext extracts the team from the request context, or returns "default".
func TeamFromContext(ctx context.Context) string {
	if rc := GetRequestContext(ctx); rc != nil && rc.Team != "" {
		return rc.Team
	}
	return "default"
}

// AppFromContext extracts the app from the request context, or returns "default".
func AppFromContext(ctx context.Context) string {
	if rc := GetRequestContext(ctx); rc != nil && rc.App != "" {
		return rc.App
	}
	return "default"
}

// FeatureFromContext extracts the feature from the request context, or returns "".
func FeatureFromContext(ctx context.Context) string {
	if rc := GetRequestContext(ctx); rc != nil {
		return rc.Feature
	}
	return ""
}
