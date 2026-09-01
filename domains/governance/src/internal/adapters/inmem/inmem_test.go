// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package inmem

import (
	"context"
	"errors"
	"testing"

	"github.com/aiplat/governance/internal/ports"
)

func TestIdentityDouble(t *testing.T) {
	ctx := context.Background()
	id := NewIdentity()

	if err := id.CreateUser(ctx, ports.User{Email: "A@x.com", Org: "acme", Role: "owner"}, "pw"); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive e-mail: recreating must collide.
	if err := id.CreateUser(ctx, ports.User{Email: "a@x.com", Org: "acme", Role: "dev"}, "pw"); !errors.Is(err, ports.ErrUserExists) {
		t.Fatalf("expected ErrUserExists, got %v", err)
	}
	org, ok, _ := id.GetUserOrg(ctx, "a@x.com")
	if !ok || org != "acme" {
		t.Fatalf("GetUserOrg=(%q,%v)", org, ok)
	}
	if err := id.UpdateAttrs(ctx, "a@x.com", map[string]string{"custom:role": "admin"}); err != nil {
		t.Fatal(err)
	}
	users, _ := id.ListUsers(ctx, "acme")
	if len(users) != 1 || users[0].Role != "admin" {
		t.Fatalf("unexpected ListUsers: %+v", users)
	}
	if users, _ := id.ListUsers(ctx, "other"); len(users) != 0 {
		t.Fatalf("org other should have no members: %+v", users)
	}
	id.DeleteUser(ctx, "a@x.com")
	if _, ok, _ := id.GetUserOrg(ctx, "a@x.com"); ok {
		t.Fatal("the user should have been removed")
	}
}

func TestSecretStoreDouble(t *testing.T) {
	ctx := context.Background()
	ss := NewSecretStore()
	name, err := ss.Put(ctx, "aiplat/org/acme/openai", "sk-123")
	if err != nil || name != "aiplat/org/acme/openai" {
		t.Fatalf("Put=(%q,%v)", name, err)
	}
	if ss.Secrets["aiplat/org/acme/openai"] != "sk-123" {
		t.Fatal("secret was not stored")
	}
}
