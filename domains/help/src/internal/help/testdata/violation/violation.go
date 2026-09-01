// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Negative case for the boundary scanner. NOT part of the build: it lives under
// testdata/, which the Go toolchain ignores.
//
// It exists so the scanner itself is tested. A boundary verifier that never sees a
// violation is indistinguishable from one that is broken — and the broken one
// passes forever while quietly protecting nothing.
//
// The two imports below are the exact shortcut this domain must never take:
// reading the markdown from disk instead of receiving it from the adapter. That
// works on a laptop and fails in Lambda, after deploy.
package violation

import (
	"net/http"
	"os"
)

func Violate() (string, error) {
	b, err := os.ReadFile("faq/overview.md")
	if err != nil {
		return "", err
	}
	_, _ = http.Get("https://example.invalid/help")
	return string(b), nil
}
