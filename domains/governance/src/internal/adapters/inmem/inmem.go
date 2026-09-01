// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package inmem holds in-memory doubles of ports.Identity and ports.SecretStore,
// used to test config-api's orchestration without a real Cognito or Secrets
// Manager (hexagonal-refactor, task 11.4; D2 — a double to test orchestration).
package inmem

import (
	"context"
	"errors"
	"strings"

	"github.com/aiplat/governance/internal/ports"
)

// Identity is an in-memory double of ports.Identity.
type Identity struct {
	Users map[string]ports.User // key: e-mail (lowercase)
	// Optional hooks to force errors in a test.
	FailCreate error
}

func NewIdentity() *Identity { return &Identity{Users: map[string]ports.User{}} }

var _ ports.Identity = (*Identity)(nil)

func (m *Identity) key(email string) string { return strings.ToLower(email) }

func (m *Identity) ListUsers(_ context.Context, org string) ([]ports.User, error) {
	var out []ports.User
	for _, u := range m.Users {
		if u.Org == org {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *Identity) CreateUser(_ context.Context, u ports.User, password string) error {
	if m.FailCreate != nil {
		return m.FailCreate
	}
	k := m.key(u.Email)
	if _, ok := m.Users[k]; ok {
		return ports.ErrUserExists
	}
	if u.Status == "" {
		if password != "" {
			u.Status = "CONFIRMED"
		} else {
			u.Status = "FORCE_CHANGE_PASSWORD"
		}
	}
	m.Users[k] = u
	return nil
}

func (m *Identity) UpdateAttrs(_ context.Context, email string, attrs map[string]string) error {
	k := m.key(email)
	u, ok := m.Users[k]
	if !ok {
		return nil
	}
	if r, ok := attrs["custom:role"]; ok {
		u.Role = r
	}
	if o, ok := attrs["custom:org_id"]; ok {
		u.Org = o
	}
	if n, ok := attrs["name"]; ok {
		u.Name = n
	}
	m.Users[k] = u
	return nil
}

func (m *Identity) DeleteUser(_ context.Context, email string) error {
	delete(m.Users, m.key(email))
	return nil
}

func (m *Identity) GetUserOrg(_ context.Context, email string) (string, bool, error) {
	u, ok := m.Users[m.key(email)]
	if !ok {
		return "", false, nil
	}
	return u.Org, true, nil
}

// ResetPassword: in the double it only marks RESET_REQUIRED (no real e-mail).
func (m *Identity) ResetPassword(_ context.Context, email string) error {
	k := m.key(email)
	if u, ok := m.Users[k]; ok {
		u.Status = "RESET_REQUIRED"
		m.Users[k] = u
	}
	return nil
}

// ResendInvite: a no-op in the double (the real invite belongs to Cognito).
func (m *Identity) ResendInvite(_ context.Context, email string) error {
	if _, ok := m.Users[m.key(email)]; !ok {
		return nil
	}
	return nil
}

// SetEnabled toggles the enabled flag in memory.
func (m *Identity) SetEnabled(_ context.Context, email string, enabled bool) error {
	k := m.key(email)
	if u, ok := m.Users[k]; ok {
		u.Enabled = enabled
		m.Users[k] = u
	}
	return nil
}

// SecretStore is an in-memory double of ports.SecretStore.
type SecretStore struct {
	Secrets map[string]string // name → api_key
	FailPut error
}

func NewSecretStore() *SecretStore { return &SecretStore{Secrets: map[string]string{}} }

var _ ports.SecretStore = (*SecretStore)(nil)

func (m *SecretStore) Put(_ context.Context, name, apiKey string) (string, error) {
	if m.FailPut != nil {
		return "", m.FailPut
	}
	m.Secrets[name] = apiKey
	return name, nil
}

func (m *SecretStore) Get(_ context.Context, name string) (string, error) {
	if v, ok := m.Secrets[name]; ok {
		return v, nil
	}
	return "", errors.New("secret not found")
}
