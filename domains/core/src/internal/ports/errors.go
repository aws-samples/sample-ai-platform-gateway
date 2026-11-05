// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

package ports

import "fmt"

// ProviderError is a provider HTTP failure that carries the status code AS DATA.
//
// Why a typed error and not a formatted string: the gateway's classifier used to grep
// the error message for substrings like "http 400" / "statuscode: 400" to decide
// whether a failure was the caller's fault (a 4xx → stop, 400 to the client, out of the
// SLI) or a provider outage (5xx → advance the chain, 502, counts against the SLI). The
// adapters, however, format their errors as "<label> <status>: <body>" (e.g.
// "anthropic 400: {...}"), which contains none of those tokens — so a provider 400 fell
// through to "provider_error" → 502 and was retried against every other model with the
// same bad body (CORE-BND-002, root of CORE-FUN-001). Carrying the status code lets the
// classifier decide on the number, with the message grep kept only as a fallback for
// errors that are not ProviderError (e.g. the Bedrock SDK's ValidationException text).
//
// Error() preserves the exact "<label> <status>: <body>" format the adapters used, so
// operator logs and any string-based assertion are unchanged.
type ProviderError struct {
	Status int    // HTTP status returned by the provider
	Label  string // provider label for the message ("anthropic", "gemini", "provider"…)
	Body   string // raw provider body (operator-only; never returned to the client)
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s %d: %s", e.Label, e.Status, e.Body)
}
