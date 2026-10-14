package dashboard_test

import (
	"embed"
	"io/fs"
	"strings"
	"testing"
)

// SCHED-GAP-1588: the dashboard tables must fit the viewport without a
// horizontal scrollbar. The load-bearing CSS lives in layout.html; these
// tests pin the properties the fix depends on so a future edit cannot
// silently reintroduce the slider.
//
//go:embed templates/layout.html
var layoutFS embed.FS

func layoutCSS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(layoutFS, "templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	return string(b)
}

// TestGap1588_TableLayoutFixed pins table-layout:fixed on the bare table
// rule — the property that makes a table exactly its container's width so
// 12 columns compress instead of overflowing.
func TestGap1588_TableLayoutFixed(t *testing.T) {
	css := layoutCSS(t)
	if !strings.Contains(css, "table-layout:fixed") {
		t.Errorf("layout.html lost table-layout:fixed — dashboard tables will overflow horizontally again")
	}
}

// TestGap1588_CellWrap pins overflow-wrap (and the legacy word-wrap alias)
// on th,td — without it long unbreakable lane names stretch a fixed-layout
// column past the viewport.
func TestGap1588_CellWrap(t *testing.T) {
	css := layoutCSS(t)
	if !strings.Contains(css, "overflow-wrap:break-word") || !strings.Contains(css, "word-wrap:break-word") {
		t.Errorf("layout.html lost overflow-wrap/word-wrap break-word on th,td — long lane names will overflow")
	}
}

// TestGap1588_ContainerScroll pins overflow:auto on .table-wrap so even a
// pathological cell (e.g. a future unstyled pre) degrades to a scroll inside
// the card instead of blowing out the page.
func TestGap1588_ContainerScroll(t *testing.T) {
	css := layoutCSS(t)
	if !strings.Contains(css, ".table-wrap{background:var(--surface);border:1px solid var(--border);border-radius:10px;overflow:auto}") {
		t.Errorf(".table-wrap lost overflow:auto — page-level horizontal scroll becomes possible")
	}
}

// TestGap1588_SidebarWidth pins the fixed 216px sidebar custom property.
func TestGap1588_SidebarWidth(t *testing.T) {
	css := layoutCSS(t)
	if !strings.Contains(css, "--sidebar-w:216px") {
		t.Errorf("layout.html lost --sidebar-w:216px — sidebar width is unpinned")
	}
}
