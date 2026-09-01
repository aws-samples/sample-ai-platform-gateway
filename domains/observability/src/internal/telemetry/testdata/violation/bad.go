// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// NEGATIVE case of the Observability Boundary Verifier.
//
// It violates the boundary on purpose: it exists only to prove the scanner complains.
// It lives in testdata/, which `go build` ignores, so it never enters any binary.
package violation

import (
	"net/http"
	"os"
)

func Bad() (*http.Client, string) {
	return http.DefaultClient, os.Getenv("SOMETHING")
}
