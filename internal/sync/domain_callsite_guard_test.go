package sync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPostMemoryCallSitesUseCanonicalDomains — the alias table is a wire
// compatibility shim for domains already in flight, NOT a licence to invent
// local domain names. Every literal postMemory call site in this package must
// name a value from the allowlist, so a future typo fails here loudly instead
// of being silently remapped (or 400ing) in production.
func TestPostMemoryCallSitesUseCanonicalDomains(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package dir: %v", err)
	}

	checked := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "postMemory" {
					return true
				}
				if len(call.Args) < 3 {
					return true
				}
				lit, ok := call.Args[2].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Errorf("%s: postMemory domain argument is not a string literal, so it cannot be checked statically — pass a value from duckbrainDomains",
						fset.Position(call.Pos()))
					return true
				}
				domain, uErr := strconv.Unquote(lit.Value)
				if uErr != nil {
					t.Errorf("%s: unquote domain literal: %v", fset.Position(lit.Pos()), uErr)
					return true
				}
				checked++
				if !domainAllowed(domain) {
					t.Errorf("%s: postMemory domain %q is not in duckbrainDomains (%s) — use a canonical value; the alias table is for values already in flight, not for new call sites",
						fset.Position(lit.Pos()), domain, allowedDomains())
				}
				return true
			})
		}
	}

	// Non-vacuity: if the walk stops finding call sites the guard would pass
	// while checking nothing.
	if checked < 7 {
		t.Errorf("checked %d postMemory call sites, want at least 7 — guard is vacuous", checked)
	}
}
