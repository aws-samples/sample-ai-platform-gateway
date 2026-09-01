// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package ports

import "context"

// Embedder turns text into a vector (embedding) for the semantic cache. It is an
// outbound BOUNDARY: the production implementation calls Bedrock (Titan), but the
// domain and the handler only know this interface — testable with a double and
// without network access.
//
// Returns an error instead of a silently empty vector: the caller treats an
// embedding failure as "no semantic cache on this request" (graceful
// degradation), never as a panic nor as a match.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
