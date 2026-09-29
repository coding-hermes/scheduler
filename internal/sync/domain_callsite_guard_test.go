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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, pErr := parser.ParseFile(fset, name, nil, 0)
		if pErr != nil {
			t.Fatalf("parse %s: %v", name, pErr)
		}
		files++
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

	// Non-vacuity: if the walk stops finding call sites the guard would pass
	// while checking nothing.
	if files == 0 {
		t.Fatal("parsed 0 non-test Go files — the guard walked nothing")
	}
	if checked < 7 {
		t.Errorf("checked %d postMemory call sites (in %d files), want at least 7 — guard is vacuous", checked, files)
	}
}
