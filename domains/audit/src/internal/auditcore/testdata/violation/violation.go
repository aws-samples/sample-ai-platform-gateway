// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// NEGATIVE case of the boundary verifier. This file violates the boundary on
// purpose: it imports net/http and os, which the pure domain must not reach.
//
// It lives in testdata/ so the module's compiler ignores it (Go does not compile
// anything under testdata), while the boundary_test scanner can still read it with
// build.ImportDir. It is the "test of the test": if the scanner breaks and stops
// flagging anything, this file makes the test fail.
package violation

import (
	"net/http"
	"os"
)

func Violate() string {
	_ = http.DefaultClient
	return os.Getenv("QUALQUER_COISA")
}
