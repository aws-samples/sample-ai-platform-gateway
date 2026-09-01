// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Boundary_Checker (spec intelligent-cost-routing, Req 7.2 and 7.3).
//
// Fails the build when the pure domain starts depending on network, files,
// environment or an SDK. Without it, "hexagonal" is just a folder name: one hurried
// commit importing the SDK here and testability without the cloud would die
// silently.
//
// Three decisions that make this test worth something:
//
//   - ALLOWLIST, not denylist. A list of forbidden packages ages badly — someone
//     would import a new SDK and the test would pass. An allowlist fails by default,
//     which is the correct behavior at a boundary.
//   - TRANSITIVE CLOSURE. Checking only direct imports leaves the obvious hole: an
//     internal helper that imports "os".
//   - This file is `package routing_test` (an EXTERNAL test package) on purpose: it
//     itself imports go/build, os and path/filepath, which are among the forbidden
//     ones. The scanner reads only p.Imports (NON-test files, the ones that go into
//     the Lambda binary), so it does not report itself.
package routing_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/aiplat/core"

// allowed: PURE stdlib — nothing that does IO, reads the environment or touches the
// network.
var allowed = map[string]bool{
	"bytes": true, "encoding/json": true, "errors": true, "fmt": true,
	"math": true, "sort": true, "strconv": true, "strings": true,
	"time": true, "unicode/utf8": true, "context": true,
	// regexp is PURE (no IO, no clock, no randomness) and is what the deterministic
	// guardrails (guardrails.go) use. It goes on the allowlist.
	"regexp": true,
	// crypto/sha256 and encoding/hex are PURE, deterministic computation — the cache
	// key (cachekey.go) hashes the request material. No IO, clock or randomness, so
	// they respect the boundary.
	"crypto/sha256": true, "encoding/hex": true,
	// encoding/binary for the same reason: it converts hash bytes into an integer,
	// with no IO and no state. It is what makes the canary (canary.go)
	// DETERMINISTIC instead of random — the boundary forbids `rand`, and it is
	// precisely that ban which forces the sampling decision to be recomputable from
	// the record.
	"encoding/binary": true,
}

// allowedModulePrefixes: internal packages the domain may reach.
// `ports` is included because escalation takes ports.Provider — and ports, in turn,
// is also verified by the transitive closure below.
var allowedModulePrefixes = []string{
	modulePath + "/internal/routing",
	modulePath + "/internal/ports",
}

func TestRoutingDomainHasNoInfrastructureImports(t *testing.T) {
	seen := map[string]bool{}
	queue := []string{modulePath + "/internal/routing"}

	for len(queue) > 0 {
		pkgPath := queue[0]
		queue = queue[1:]
		if seen[pkgPath] {
			continue
		}
		seen[pkgPath] = true

		dir, err := resolveDir(pkgPath)
		if err != nil {
			t.Fatalf("could not resolve %s: %v", pkgPath, err)
		}
		p, err := build.ImportDir(dir, 0)
		if err != nil {
			t.Fatalf("could not read %s: %v", dir, err)
		}

		// p.Imports = imports of the NON-test files. That is the boundary that
		// matters: what actually goes into the Lambda binary.
		for _, imp := range p.Imports {
			switch {
			case allowed[imp]:
				continue
			case isAllowedInternal(imp):
				queue = append(queue, imp)
			default:
				t.Errorf("boundary violated: %s imports %q (outside the allowlist)", pkgPath, imp)
			}
		}
	}
}

// TestRoutingDomainDoesNotReadTheClock: `time` is allowed as a TYPE (the reference
// instant comes in as a parameter), but reading the clock inside the domain would
// break the determinism required by Req 2.7 and would make price validity and credit
// expiration impossible to test.
func TestRoutingDomainDoesNotReadTheClock(t *testing.T) {
	dir, err := resolveDir(modulePath + "/internal/routing")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// package → forbidden function prefix ("" = any use of the package)
	forbidden := map[string]string{"time": "Now", "rand": "", "os": ""}
	for _, p := range pkgs {
		ast.Inspect(p, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if fn, bad := forbidden[ident.Name]; bad && strings.HasPrefix(sel.Sel.Name, fn) {
				t.Errorf("%s: %s.%s inside the pure domain — pass the value as a parameter",
					fset.Position(sel.Pos()), ident.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestBoundaryScannerActuallyCatchesViolations is the test of the test: it points the
// scanner at a known NEGATIVE case and requires it to complain. Without this, a
// broken scanner would go unnoticed and give false confidence.
func TestBoundaryScannerActuallyCatchesViolations(t *testing.T) {
	dir := filepath.Join("testdata", "violation")
	p, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("could not read %s: %v", dir, err)
	}
	found := false
	for _, imp := range p.Imports {
		if !allowed[imp] && !isAllowedInternal(imp) {
			found = true
		}
	}
	if !found {
		t.Fatal("the scanner did not flag the negative case in testdata/violation")
	}
}

// TestNoCorePackageImportsAnotherDomain (hexagonal-refactor, task 7.2, R9.1,
// Property 6): scans ALL Core packages (cmd/... and internal/...) and fails if any of
// them imports another platform domain (github.com/aiplat/<x> with x != core). It is
// what prevents a shortcut into synchronous coupling between domains — the golden
// rule of aiplat-domains.md — and guarantees no shared runtime package is born
// between domains.
//
// It also covers this phase's new files (guardrails.go, record.go) by construction:
// they live in internal/routing, one of the scanned packages.
func TestNoCorePackageImportsAnotherDomain(t *testing.T) {
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	const otherDomainPrefix = "github.com/aiplat/"
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); name == "testdata" || name == ".git" {
			return filepath.SkipDir
		}
		p, ierr := build.ImportDir(path, 0)
		if ierr != nil {
			return nil // directory with no Go package: skip it
		}
		// Test imports are included too: not even a _test.go may couple domains.
		imports := append(append([]string{}, p.Imports...), p.TestImports...)
		for _, imp := range imports {
			if strings.HasPrefix(imp, otherDomainPrefix) && !strings.HasPrefix(imp, modulePath) {
				t.Errorf("package %s imports another domain: %q", p.ImportPath, imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// moduleRoot returns the module root (the src/ directory), two levels above
// internal/routing (where the test runs).
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Join(wd, "..", "..")), nil
}

func isAllowedInternal(imp string) bool {
	for _, pre := range allowedModulePrefixes {
		if imp == pre || strings.HasPrefix(imp, pre+"/") {
			return true
		}
	}
	return false
}

// resolveDir converts a module package path into a relative directory, without
// depending on GOPATH or on module resolution (which would require the network).
func resolveDir(pkgPath string) (string, error) {
	rel := strings.TrimPrefix(pkgPath, modulePath+"/")
	wd, err := os.Getwd() // wd = .../internal/routing
	if err != nil {
		return "", err
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}
