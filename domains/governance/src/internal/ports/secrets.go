// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package ports

import "context"

// SecretStore stores provider credentials. Least privilege: the shell only builds
// names under aiplat/gateway/* (platform) and aiplat/org/<org>/* (the org's BYO).
// The port receives the FULL NAME already resolved — the prefix/scope policy
// belongs to the shell (it depends on the token's role and org), not to the
// adapter.
type SecretStore interface {
	// Put stores (creates or updates) the secret under the exact name provided and
	// returns the stored name. The value is serialized as {"api_key": <apiKey>}.
	Put(ctx context.Context, name, apiKey string) (string, error)

	// Get reads the secret's api_key under the exact name provided. Used to list
	// the provider's models with the org's credential WITHOUT it leaving the
	// backend (the customer's app never sees the key). Errors when
	// absent/unreadable.
	Get(ctx context.Context, name string) (string, error)
}
