// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0

// Boundary verifier for the Help pure domain.
//
// Closes a real gap: `hexagonal-refactor` R5.1 requires every domain with a pure
// core to have this test, and `help` shipped without one. So the rule was
// unenforced here — the thinnest domain is exactly where an `os.ReadFile` slips in
// "just to load the markdown", which is the mistake `embedstore` exists to prevent.
//
// Three decisions, same as the other four domains:
//
//   - ALLOWLIST, not denylist: fails by default, which is the correct posture at a
//     boundary. A denylist would silently pass the next SDK someone imports.
//   - TRANSITIVE CLOSURE: the whole import graph of non-test files, not just direct
//     imports — otherwise a helper package is a free hole.
//   - This file is `package help_test` (EXTERNAL test package) on purpose: it
//     itself imports go/build, os and path/filepath, all forbidden inside the
//     domain. The scanner reads only `p.Imports` (files that reach the binary), so
//     it never reports itself.
package help_test

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

const modulePath = "github.com/aiplat/help"

// allowed: PURE stdlib — nothing that does IO, reads the environment or touches
// the network. This domain needs very little: it resolves a (lang, key) pair
// against a catalog handed to it. If this list has to grow, that is the signal to
// ask whether the logic belongs in the adapter instead.
var allowed = map[string]bool{
	"errors": true, "fmt": true, "sort": true, "strings": true,
	"unicode/utf8": true,
}

// allowedModulePrefixes: `ports` is reachable because the domain speaks in terms
// of the content port; ports is itself covered by the transitive closure below.
var allowedModulePrefixes = []string{
	modulePath + "/internal/help",
	modulePath + "/internal/ports",
}

func TestHelpDomainHasNoInfrastructureImports(t *testing.T) {
	seen := map[string]bool{}
	queue := []string{modulePath + "/internal/help"}

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
				t.Errorf("boundary violated: %s imports %q (not in the allowlist)", pkgPath, imp)
			}
		}
	}
}

// TestHelpDomainReadsNoFileNorClock is the one that matters most for THIS domain.
//
// Help content is markdown, and the tempting shortcut is `os.ReadFile`. That would
// work on a laptop and fail in Lambda, where the content is embedded in the binary
// — a class of bug that only shows up after deploy. `embed` lives in the adapter;
// the domain receives a catalog and never learns where the bytes came from.
func TestHelpDomainReadsNoFileNorClock(t *testing.T) {
	dir, err := resolveDir(modulePath + "/internal/help")
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
	// package -> forbidden function prefix ("" = any use of the package at all)
	forbidden := map[string]string{"os": "", "embed": "", "rand": "", "time": "Now"}
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
				t.Errorf("%s: %s.%s inside the pure domain — take the value as a parameter",
					fset.Position(sel.Pos()), ident.Name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestBoundaryScannerActuallyCatchesViolations is the test of the test: point the
// scanner at a known NEGATIVE case and require it to complain. Without this, a
// broken scanner passes forever and hands out false confidence.
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

// TestNoHelpPackageImportsAnotherDomain: no domain imports another. Help is the
// likeliest place to break this, because its content explains the OTHER domains
// and importing their constants to build a title would feel harmless.
func TestNoHelpPackageImportsAnotherDomain(t *testing.T) {
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
			return nil // directory with no Go package: skip
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
