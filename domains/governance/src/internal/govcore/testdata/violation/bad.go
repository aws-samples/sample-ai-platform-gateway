// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// NEGATIVE case for the Governance Boundary Verifier.
//
// It violates the boundary on purpose: it exists only to prove the scanner flags
// it. It lives in testdata/, which `go build` ignores, so it never ends up in any
// binary.
package violation

import (
	"net/http"
	"os"
)

func Bad() (*http.Client, string) {
	return http.DefaultClient, os.Getenv("SOMETHING")
}
