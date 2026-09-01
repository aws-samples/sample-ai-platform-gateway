// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Package ports declares the outbound boundaries of the Help domain.
//
// Why this is a port (hexagonal): ContentStore inverts the dependency on the
// content source. Today it is go:embed; tomorrow it could be DynamoDB (editing
// without a redeploy) without touching the pure domain or the shell. It also
// allows a test double.
package ports

import (
	"context"

	"github.com/aiplat/help/internal/help"
)

// ContentStore loads the help content of EVERY language in one call.
//
// Loading everything (rather than Load(ctx, lang)) is deliberate: the content is
// embedded in the binary, so it is already in memory, and having the whole Bundle
// is what makes the fallback chain resolvable inside the pure domain. If the
// source ever becomes remote, the adapter starts caching — the signature does not
// have to change.
type ContentStore interface {
	Load(ctx context.Context) (help.Bundle, error)
}
