package api_test

// SCHED-GAP-1700 — the lane relationship map over REST (/api/v1/links).
//
// The API contract under test:
//
//	POST   /api/v1/links                              record parent→child
//	GET    /api/v1/links                              list / filter
//	DELETE /api/v1/links?parent=X&child=Y             remove
//	GET    /api/v1/links/parents?lane=X               who are X's parents
//	GET    /api/v1/links/children?lane=X              who are X's children
//	GET    /api/v1/links/ancestors?lane=X             transitive parents
//	GET    /api/v1/links/descendants?lane=X           transitive children
//	GET    /api/v1/links/ancestor?lane=X&ancestor=Y   is Y an ancestor of X
//
// The acceptance this file pins: many parents AND many children per lane,
// cross-namespace endpoints, each lookup answered in ONE call, and every
// refusal carrying a NAMED error code (cycle, unknown_lane, self_link,
// link_exists, link_not_found) instead of one generic 400.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// seed1700API creates the lanes (and optional namespace binding) the API
// tests link together.
func seed1700API(t *testing.T, a *apiTestServer, ns string, names ...string) {
	t.Helper()
	for _, n := range names {
		mustCreateAPITestProject(t, a.db, n)
		if ns == "" {
			continue
		}
		if _, err := a.db.ExecContext(context.Background(),
			`UPDATE projects SET namespace_id = ? WHERE name = ?`, ns, n); err != nil {
			t.Fatalf("bind %s to %s: %v", n, ns, err)
		}
	}
}

// seedNamespace1700 creates a namespace row.
func seedNamespace1700(t *testing.T, a *apiTestServer, id string) {
	t.Helper()
	if err := database.CreateNamespace(context.Background(), a.db, &database.Namespace{
		ID: id, Weight: 10, Enabled: true, Description: "SCHED-GAP-1700 fixture",
	}); err != nil {
		t.Fatalf("CreateNamespace %s: %v", id, err)
	}
}

// linksOf pulls the "links" array out of a list response.
func linksOf(t *testing.T, body map[string]interface{}) []interface{} {
	t.Helper()
	raw, ok := body["links"].([]interface{})
	if !ok {
		t.Fatalf("response carries no links array: %v", body)
	}
	return raw
}

// postLink is the create helper: it asserts the 201 and returns the link.
func postLink(t *testing.T, a *apiTestServer, parent, child, note string) map[string]interface{} {
	t.Helper()
	status, body := a.do(t, "POST", "/api/v1/links", map[string]string{
		"parent_lane": parent, "child_lane": child, "note": note,
	})
	if status != http.StatusCreated {
		t.Fatalf("POST links %s→%s = %d (%v), want 201", parent, child, status, body)
	}
	link, ok := body["link"].(map[string]interface{})
	if !ok {
		t.Fatalf("POST links response has no link object: %v", body)
	}
	return link
}

// TestSCHEDGAP1700_LinksAPI_RoundTrip is the full acceptance-1/2 walk:
func TestSCHEDGAP1700_LinksAPI_RoundTrip(t *testing.T) {
	a := newAPITestServer(t)
	seedNamespace1700(t, a, "ns1700-alpha")
	seedNamespace1700(t, a, "ns1700-beta")
	seed1700API(t, a, "ns1700-alpha", "api1700-a", "api1700-b", "api1700-d")
	seed1700API(t, a, "ns1700-beta", "api1700-c")

	link := postLink(t, a, "api1700-a", "api1700-c", "cross-namespace reference")
	postLink(t, a, "api1700-b", "api1700-c", "")
	postLink(t, a, "api1700-c", "api1700-d", "")
	postLink(t, a, "api1700-a", "api1700-d", "")

	// (1) round-trip: the created edge echoes its endpoints + derived
	// namespaces, snake_case per S06.
	if link["parent_lane"] != "api1700-a" || link["child_lane"] != "api1700-c" {
		t.Errorf("created link = %v, want api1700-a→api1700-c", link)
	}
	if link["parent_namespace"] != "ns1700-alpha" || link["child_namespace"] != "ns1700-beta" {
		t.Errorf("created link namespaces = (%v,%v), want (ns1700-alpha,ns1700-beta)",
			link["parent_namespace"], link["child_namespace"])
	}
	if link["cross_namespace"] != true {
		t.Errorf("cross_namespace = %v, want true for a two-namespace edge", link["cross_namespace"])
	}
	if link["note"] != "cross-namespace reference" {
		t.Errorf("note = %v, want the submitted text", link["note"])
	}
	if link["created_at"] == "" || link["created_at"] == nil {
		t.Errorf("created_at = %v, want a timestamp", link["created_at"])
	}

	// (2) list + filter
	status, list := a.do(t, "GET", "/api/v1/links", nil)
	if status != http.StatusOK {
		t.Fatalf("GET links = %d", status)
	}
	if got := linksOf(t, list); len(got) != 4 {
		t.Errorf("GET /api/v1/links = %d edges, want 4", len(got))
	}
	status, touching := a.do(t, "GET", "/api/v1/links?lane=api1700-c", nil)
	if status != http.StatusOK {
		t.Fatalf("GET links?lane=c = %d", status)
	}
	if got := linksOf(t, touching); len(got) != 3 {
		t.Errorf("edges touching api1700-c = %d, want 3", len(got))
	}

	// (3) who are c's parents — ONE call, TWO parents
	status, parents := a.do(t, "GET", "/api/v1/links/parents?lane=api1700-c", nil)
	if status != http.StatusOK {
		t.Fatalf("GET parents = %d (%v)", status, parents)
	}
	pl, _ := parents["parents"].([]interface{})
	if len(pl) != 2 {
		t.Fatalf("c's parents = %v, want 2", parents["parents"])
	}
	seen := map[string]bool{}
	for _, raw := range pl {
		m, _ := raw.(map[string]interface{})
		seen[m["parent_lane"].(string)] = true
	}
	if !seen["api1700-a"] || !seen["api1700-b"] {
		t.Errorf("c's parent lanes = %v, want api1700-a and api1700-b", seen)
	}

	// (4) who are a's children — ONE call, TWO children
	status, children := a.do(t, "GET", "/api/v1/links/children?lane=api1700-a", nil)
	if status != http.StatusOK {
		t.Fatalf("GET children = %d (%v)", status, children)
	}
	cl, _ := children["children"].([]interface{})
	if len(cl) != 2 {
		t.Errorf("a's children = %v, want 2", children["children"])
	}

	// (5) ancestors of d — one call, depth-annotated
	status, ancestors := a.do(t, "GET", "/api/v1/links/ancestors?lane=api1700-d", nil)
	if status != http.StatusOK {
		t.Fatalf("GET ancestors = %d (%v)", status, ancestors)
	}
	al, _ := ancestors["ancestors"].([]interface{})
	if len(al) != 3 {
		t.Errorf("d's ancestors = %v, want 3 (a, b, c)", ancestors["ancestors"])
	}
	if int(ancestors["count"].(float64)) != len(al) {
		t.Errorf("count = %v, want %d", ancestors["count"], len(al))
	}

	// (6) descendants of a
	status, descendants := a.do(t, "GET", "/api/v1/links/descendants?lane=api1700-a", nil)
	if status != http.StatusOK {
		t.Fatalf("GET descendants = %d (%v)", status, descendants)
	}
	dl, _ := descendants["descendants"].([]interface{})
	if len(dl) != 2 {
		t.Errorf("a's descendants = %v, want 2 (c, d)", descendants["descendants"])
	}

	// (7) is b an ancestor of d — ONE call, with the witness path
	status, anc := a.do(t, "GET", "/api/v1/links/ancestor?lane=api1700-d&ancestor=api1700-b", nil)
	if status != http.StatusOK {
		t.Fatalf("GET ancestor = %d (%v)", status, anc)
	}
	if anc["is_ancestor"] != true {
		t.Errorf("is_ancestor(b→d) = %v, want true", anc["is_ancestor"])
	}
	path, _ := anc["path"].([]interface{})
	if len(path) != 3 || path[0] != "api1700-b" || path[2] != "api1700-d" {
		t.Errorf("witness path = %v, want [api1700-b … api1700-d]", anc["path"])
	}
	if int(anc["hops"].(float64)) != 2 {
		t.Errorf("hops = %v, want 2", anc["hops"])
	}
	// The reverse direction must be false — and it is answered, not refused.
	status, rev := a.do(t, "GET", "/api/v1/links/ancestor?lane=api1700-b&ancestor=api1700-d", nil)
	if status != http.StatusOK || rev["is_ancestor"] != false {
		t.Errorf("is_ancestor(d→b) = (%d, %v), want (200,false)", status, rev["is_ancestor"])
	}

	// (8) delete removes exactly that edge; the map shrinks by one.
	status, del := a.do(t, "DELETE", "/api/v1/links?parent=api1700-a&child=api1700-d", nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE edge = %d (%v)", status, del)
	}
	if del["status"] != "deleted" {
		t.Errorf("DELETE status = %v, want deleted", del["status"])
	}
	_, after := a.do(t, "GET", "/api/v1/links", nil)
	if got := linksOf(t, after); len(got) != 3 {
		t.Errorf("edges after delete = %d, want 3", len(got))
	}
	_, childrenAfter := a.do(t, "GET", "/api/v1/links/children?lane=api1700-a", nil)
	if cl, _ := childrenAfter["children"].([]interface{}); len(cl) != 1 {
		t.Errorf("a's children after delete = %v, want 1", childrenAfter["children"])
	}
}

// TestSCHEDGAP1700_LinksAPI_NamedErrors is acceptance 4 over the wire: every
// refusal names its class in a `code` field, and the status matches the class.
func TestSCHEDGAP1700_LinksAPI_NamedErrors(t *testing.T) {
	a := newAPITestServer(t)
	seed1700API(t, a, "", "api1700-e-a", "api1700-e-b", "api1700-e-c")
	postLink(t, a, "api1700-e-a", "api1700-e-b", "")
	postLink(t, a, "api1700-e-b", "api1700-e-c", "")

	cases := []struct {
		name       string
		method     string
		path       string
		body       interface{}
		wantStatus int
		wantCode   string
	}{
		{"unknown parent lane", "POST", "/api/v1/links",
			map[string]string{"parent_lane": "api1700-ghost", "child_lane": "api1700-e-a"},
			http.StatusNotFound, "unknown_lane"},
		{"unknown child lane", "POST", "/api/v1/links",
			map[string]string{"parent_lane": "api1700-e-a", "child_lane": "api1700-ghost"},
			http.StatusNotFound, "unknown_lane"},
		{"self link", "POST", "/api/v1/links",
			map[string]string{"parent_lane": "api1700-e-a", "child_lane": "api1700-e-a"},
			http.StatusBadRequest, "self_link"},
		{"cycle", "POST", "/api/v1/links",
			map[string]string{"parent_lane": "api1700-e-c", "child_lane": "api1700-e-a"},
			http.StatusConflict, "cycle"},
		{"duplicate edge", "POST", "/api/v1/links",
			map[string]string{"parent_lane": "api1700-e-a", "child_lane": "api1700-e-b"},
			http.StatusConflict, "link_exists"},
		{"delete missing edge", "DELETE",
			"/api/v1/links?parent=api1700-e-b&child=api1700-e-a", nil,
			http.StatusNotFound, "link_not_found"},
		{"delete without params", "DELETE", "/api/v1/links", nil,
			http.StatusBadRequest, "invalid_request"},
		{"parents for unknown lane", "GET", "/api/v1/links/parents?lane=api1700-ghost", nil,
			http.StatusNotFound, "unknown_lane"},
		{"parents without lane", "GET", "/api/v1/links/parents", nil,
			http.StatusBadRequest, "invalid_request"},
		{"ancestor without ancestor param", "GET", "/api/v1/links/ancestor?lane=api1700-e-a", nil,
			http.StatusBadRequest, "invalid_request"},
		{"unknown sub-resource", "GET", "/api/v1/links/bogus?lane=api1700-e-a", nil,
			http.StatusNotFound, "unknown_sub_resource"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := a.do(t, tc.method, tc.path, tc.body)
			if status != tc.wantStatus {
				t.Errorf("%s %s = %d, want %d (%v)", tc.method, tc.path, status, tc.wantStatus, body)
			}
			if body["code"] != tc.wantCode {
				t.Errorf("code = %v, want %q (%v)", body["code"], tc.wantCode, body)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Errorf("refusal carries no error message: %v", body)
			}
		})
	}

	// The refused writes left the graph untouched.
	_, list := a.do(t, "GET", "/api/v1/links", nil)
	if got := linksOf(t, list); len(got) != 2 {
		t.Errorf("edges after refusals = %d, want 2 (refusals must not write)", len(got))
	}
}

// TestSCHEDGAP1700_LinksAPI_MutationGate proves the writes are operator-gated
// (SCHED-GAP-1602) while the reads stay open, so the map cannot be edited by
// an unauthenticated caller.
func TestSCHEDGAP1700_LinksAPI_MutationGate(t *testing.T) {
	a := newAPITestServer(t)
	seed1700API(t, a, "", "api1700-g-a", "api1700-g-b")
	postLink(t, a, "api1700-g-a", "api1700-g-b", "")

	// Bare request: no operator credential at all.
	bare := func(method, path string, body string) (int, map[string]interface{}) {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, a.ts.URL+path, rdr)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var parsed map[string]interface{}
		_ = json.Unmarshal(raw, &parsed)
		return resp.StatusCode, parsed
	}

	if status, _ := bare("POST", "/api/v1/links",
		`{"parent_lane":"api1700-g-a","child_lane":"api1700-g-b"}`); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d, want 401", status)
	}
	if status, _ := bare("DELETE", "/api/v1/links?parent=api1700-g-a&child=api1700-g-b", ""); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated DELETE = %d, want 401", status)
	}
	// Reads stay open — the dashboard render path and cron probes are
	// read-only observers (auth.go ROUTE SPLIT).
	for _, path := range []string{
		"/api/v1/links",
		"/api/v1/links/parents?lane=api1700-g-b",
		"/api/v1/links/children?lane=api1700-g-a",
		"/api/v1/links/ancestors?lane=api1700-g-b",
		"/api/v1/links/descendants?lane=api1700-g-a",
		"/api/v1/links/ancestor?lane=api1700-g-b&ancestor=api1700-g-a",
	} {
		if status, _ := bare("GET", path, ""); status != http.StatusOK {
			t.Errorf("unauthenticated GET %s = %d, want 200", path, status)
		}
	}

	// The edge is still there — a refused mutation changed nothing.
	_, list := a.do(t, "GET", "/api/v1/links", nil)
	if got := linksOf(t, list); len(got) != 1 {
		t.Errorf("edges = %d after refused mutations, want 1", len(got))
	}
}
