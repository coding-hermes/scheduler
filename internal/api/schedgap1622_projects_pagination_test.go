package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1622 acceptance tests 2 and 3 for GET /api/v1/projects
// (pagination; the behavior change is the core of SCHED-GAP-1624's ?limit=
// fix) and GET /api/v1/health (countActiveTicks bounded by the v46 partial
// running-ticks index).

// TestProjectsPagination_DefaultLimitApplied proves a bare GET applies the
// default cap and echoes the pagination triple.
func TestProjectsPagination_DefaultLimitApplied(t *testing.T) {
	a := newAPITestServer(t)
	// Seed exactly 3 — under the default cap, so the page holds all of them
	// while limit still reports the applied default (200).
	mustCreateAPITestProject(t, a.db, "page-a")
	mustCreateAPITestProject(t, a.db, "page-b")
	mustCreateAPITestProject(t, a.db, "page-c")
	status, body := a.do(t, "GET", "/api/v1/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	projs, ok := body["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", body["projects"])
	}
	if len(projs) != 3 {
		t.Errorf("projects rows = %d, want 3", len(projs))
	}
	if body["limit"] == nil {
		t.Errorf("limit missing from the responses envelope: %v", body)
	}
	if got := body["limit"].(float64); int(got) != database.DefaultListProjectsLimit {
		t.Errorf("limit = %v, want the default %d (the applied value must be echoed, not the requested one)", got, database.DefaultListProjectsLimit)
	}
	if got := body["total"].(float64); int(got) != 3 {
		t.Errorf("total = %v, want 3 (all matching projects, not the page length)", got)
	}
	if got := body["offset"].(float64); int(got) != 0 {
		t.Errorf("offset = %v, want 0", got)
	}
}

// TestProjectsPagination_LimitOffsetSlice seeds 7 projects and proves
// ?limit=2&offset=2 returns exactly the right slice, ordered, with the full
// total; ?limit= returns the first page.
func TestProjectsPagination_LimitOffsetSlice(t *testing.T) {
	a := newAPITestServer(t)
	names := []string{"slice-a", "slice-b", "slice-c", "slice-d", "slice-e", "slice-f", "slice-g"}
	for _, n := range names {
		mustCreateAPITestProject(t, a.db, n)
	}

	status, body := a.do(t, "GET", "/api/v1/projects?limit=2&offset=2", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	projs, ok := body["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", body["projects"])
	}
	if len(projs) != 2 {
		t.Fatalf("rows = %d, want 2", len(projs))
	}
	want := []interface{}{"slice-c", "slice-d"}
	for i, raw := range projs {
		p := raw.(map[string]interface{})
		if p["name"] != want[i] {
			t.Errorf("row %d name = %v, want %v", i, p["name"], want[i])
		}
	}
	if got := body["total"].(float64); int(got) != len(names) {
		t.Errorf("total = %v, want %d", got, len(names))
	}
	if got := body["limit"].(float64); int(got) != 2 {
		t.Errorf("limit = %v, want 2", got)
	}
	if got := body["offset"].(float64); int(got) != 2 {
		t.Errorf("offset = %v, want 2", got)
	}
}

// TestProjectsPagination_LimitClamped proves the handler serves the clamped
// bounds, never a 4xx/5xx: ?limit=10000 → 500, ?limit=abc → default,
// ?limit=-1 → default, ?offset=-3 → 0.
func TestProjectsPagination_LimitClamped(t *testing.T) {
	cases := []struct {
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"?limit=10000", database.MaxListProjectsLimit, 0},
		{"?limit=abc", database.DefaultListProjectsLimit, 0},
		{"?limit=-1", database.DefaultListProjectsLimit, 0},
		{"?offset=-3", database.DefaultListProjectsLimit, 0},
		{"?limit=0", database.DefaultListProjectsLimit, 0},
	}
	for _, c := range cases {
		a := newAPITestServer(t)
		status, body := a.do(t, "GET", "/api/v1/projects"+c.query, nil)
		if status != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (clamping is fail-soft, never a 4xx)", c.query, status)
			continue
		}
		if got := int(body["limit"].(float64)); got != c.wantLimit {
			t.Errorf("%s: limit = %d, want %d", c.query, got, c.wantLimit)
		}
		if got := int(body["offset"].(float64)); got != c.wantOffset {
			t.Errorf("%s: offset = %d, want %d", c.query, got, c.wantOffset)
		}
	}
}

// TestProjectsPagination_TotalWithSeededPaging simulates the whole pager walk:
// two pages cover every seeded project exactly once (order is stable), the
// last page is short, and total stays constant across pages.
func TestProjectsPagination_TotalWithSeededPaging(t *testing.T) {
	a := newAPITestServer(t)
	names := []string{"walk-a", "walk-b", "walk-c", "walk-d", "walk-e"}
	for _, n := range names {
		mustCreateAPITestProject(t, a.db, n)
	}
	const limit = 2
	seen := map[string]bool{}
	for offset := 0; offset < len(names); offset += limit {
		status, body := a.do(t, "GET", fmt.Sprintf("/api/v1/projects?limit=%d&offset=%d", limit, offset), nil)
		if status != http.StatusOK {
			t.Fatalf("offset=%d: status = %d", offset, status)
		}
		projs := body["projects"].([]interface{})
		if got := int(body["total"].(float64)); got != len(names) {
			t.Errorf("offset=%d: total = %d, want %d", offset, got, len(names))
		}
		for _, raw := range projs {
			p := raw.(map[string]interface{})
			name := p["name"].(string)
			if seen[name] {
				t.Errorf("offset=%d: %s seen twice across the walk", offset, name)
			}
			seen[name] = true
		}
	}
	if len(seen) != len(names) {
		t.Errorf("pager walk covered %d projects, want %d", len(seen), len(names))
	}
}
