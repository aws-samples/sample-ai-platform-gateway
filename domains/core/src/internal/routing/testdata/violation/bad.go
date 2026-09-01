// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// NEGATIVE case for the Boundary_Verifier (Req 7.2).
//
// This file violates the boundary on purpose: it exists only to prove the scanner
// catches it. It lives under testdata/, which `go build` ignores, so it never makes
// it into the Lambda binary.
package violation

import (
	"net/http"
	"os"
)

func Bad() (*http.Client, string) {
	return http.DefaultClient, os.Getenv("SOMETHING")
}
