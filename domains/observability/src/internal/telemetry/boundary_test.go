// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Boundary Verifier of the Observability pure domain (hexagonal-refactor, R5/D5,
// Property 2 and 3). Modeled on core/internal/routing/boundary_test.go and on
// governance/internal/govcore/boundary_test.go.
//
// Fails the build when telemetry starts depending on network, files, environment,
// SDK, clock or randomness. Three decisions make this test worth something:
//
//   - ALLOWLIST, not denylist: fails by default, which is the right stance at a boundary.
//   - TRANSITIVE CLOSURE: it checks the whole import graph (non-test files).
//   - This file is package telemetry_test (an EXTERNAL test package) on purpose:
//     it imports go/build, os and path/filepath itself — all forbidden in the domain.
//     The scanner reads only p.Imports (files that go into the binary), so it does not
//     report itself.
package telemetry_test

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

const modulePath = "github.com/aiplat/observability"

// allowed: PURE stdlib — nothing that does IO, reads the environment or touches the
// network. `time` is in as a TYPE (the reference instant is a parameter); reading the
// clock is forbidden by the separate test below.
var allowed = map[string]bool{
	"bytes": true, "encoding/json": true, "errors": true, "fmt": true,
	"math": true, "sort": true, "strconv": true, "strings": true,
	"time": true, "unicode/utf8": true, "context": true, "regexp": true,
}

var allowedModulePrefixes = []string{
	modulePath + "/internal/telemetry",
}

func TestTelemetryDomainHasNoInfrastructureImports(t *testing.T) {
	seen := map[string]bool{}
	queue := []string{modulePath + "/internal/telemetry"}

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

// TestTelemetryDoesNotReadTheClock: `time` is allowed as a TYPE, but reading the
// clock (time.Now), randomness (rand) or the environment (os) inside the domain would
// break determinism — the SLI/burn/anomaly math takes the instant as a parameter.
func TestTelemetryDoesNotReadTheClock(t *testing.T) {
	dir, err := resolveDir(modulePath + "/internal/telemetry")
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

// TestBoundaryScannerActuallyCatchesViolations: the test of the test — it points the
// scanner at a known NEGATIVE case (testdata/violation) and requires it to complain.
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

// TestNoObservabilityPackageImportsAnotherDomain (hexagonal-refactor, R9.1/9.2,
// Property 6): sweeps ALL Observability packages and fails if any imports another
// platform domain (github.com/aiplat/<x> with x != observability). It is what ensures
// no shared runtime package is born between domains.
func TestNoObservabilityPackageImportsAnotherDomain(t *testing.T) {
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
			return nil
		}
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

func resolveDir(pkgPath string) (string, error) {
	rel := strings.TrimPrefix(pkgPath, modulePath+"/")
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}
