// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ports declares Governance's outbound (driven) boundaries: the
// interfaces the domain needs and the infrastructure implements.
//
// Why these are ports (D2): Identity has a double to test config-api's
// orchestration (member invite/edit/removal) and inverts the dependency at
// compile time, keeping the shell testable without a real Cognito. Same for
// SecretStore.
package ports

import (
	"context"
	"errors"
)

// ErrUserExists is the sentinel error returned by CreateUser when the e-mail
// already exists in the pool. It lets the shell map to 409 without inspecting the
// provider's error string (preserving config-api's current status contract).
var ErrUserExists = errors.New("user already exists")

// User is the identity at the Cognito level (the identity control plane).
// The member's team and apps do NOT live here — they stay in the config's MEMBER#
// record, and the merge is shell orchestration.
type User struct {
	Email   string
	Org     string
	Role    string
	Name    string
	Status  string // filled on read (e.g. CONFIRMED, FORCE_CHANGE_PASSWORD)
	Enabled bool   // account enabled in the pool (AdminEnable/DisableUser)
}

// Identity is the port of the identity control plane (today Cognito via SigV4 HTTP).
type Identity interface {
	// ListUsers returns the users whose custom:org_id == org (client-side match;
	// custom attributes are not filterable server-side). Pagination is internal.
	ListUsers(ctx context.Context, org string) ([]User, error)

	// CreateUser creates the user. When password != "", it creates with a
	// PERMANENT password and no invite e-mail (the platform_admin flow); when
	// password == "", it sends an e-mail invite with a temporary password. Returns
	// ErrUserExists when the user already exists.
	CreateUser(ctx context.Context, u User, password string) error

	// UpdateAttrs updates the user's attributes (e.g. custom:role).
	UpdateAttrs(ctx context.Context, email string, attrs map[string]string) error

	// DeleteUser removes the user from the pool.
	DeleteUser(ctx context.Context, email string) error

	// GetUserOrg reads a user's custom:org_id (ownership check).
	// ok=false when the read could not confirm the org (an error, or a user without
	// the attribute).
	GetUserOrg(ctx context.Context, email string) (org string, ok bool, err error)

	// ResetPassword triggers the e-mail reset flow (Cognito sends a code; the
	// member picks the new password). An admin NEVER sees or sets someone else's
	// password — which is why there is no SetPassword for a member on this port.
	ResetPassword(ctx context.Context, email string) error

	// ResendInvite resends the invite (a new temporary password by e-mail) to a
	// member who has not activated the account yet.
	ResendInvite(ctx context.Context, email string) error

	// SetEnabled enables or disables the account in the pool. A disabled account
	// cannot authenticate (reversible block, without deleting the user).
	SetEnabled(ctx context.Context, email string, enabled bool) error
}
