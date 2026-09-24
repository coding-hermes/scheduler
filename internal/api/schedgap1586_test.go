package api_test

import (
	"testing"
)

// SCHED-GAP-1586 — lane parent reference over the project API surface.
//
// parent is settable through PUT /api/v1/projects/{name} exactly like
// deliver: a free-form lane name stored as given, "" clearing back to a
// primary/root lane. A reference that would make the lane its own ancestor
// (self-parent or any longer loop) is a client-correctable 400, and a
// rejected PUT must leave the stored parent untouched.

// TestSCHEDGAP1586_ParentAPI covers set → GET → clear, plus the 400 shapes
// (self-parent, indirect cycle) and the no-side-effect guarantee of a
// rejected write.
func TestSCHEDGAP1586_ParentAPI(t *testing.T) {
	a := newAPITestServer(t)
	mustCreateAPITestProject(t, a.db, "1586-root")
	mustCreateAPITestProject(t, a.db, "1586-mid")
	mustCreateAPITestProject(t, a.db, "1586-leaf")

	// Set leaf.parent = mid through the API.
	code, body := a.do(t, "PUT", "/api/v1/projects/1586-leaf", map[string]interface{}{"parent": "1586-mid"})
	if code != 200 {
		t.Fatalf("PUT parent=1586-mid: status %d body %v", code, body)
	}
	if body["parent"] != "1586-mid" {
		t.Errorf("PUT response parent = %v, want 1586-mid", body["parent"])
	}
	// Read it back through a bare GET (the detail handler nests the
	// project under "project").
	code, body = a.do(t, "GET", "/api/v1/projects/1586-leaf", nil)
	if code != 200 {
		t.Fatalf("GET after set: status %d body %v", code, body)
	}
	proj := body["project"].(map[string]interface{})
	if proj["parent"] != "1586-mid" {
		t.Fatalf("GET after set: parent %v, want 1586-mid", proj["parent"])
	}

	// Self-parenting is a 400.
	code, body = a.do(t, "PUT", "/api/v1/projects/1586-leaf", map[string]interface{}{"parent": "1586-leaf"})
	if code != 400 {
		t.Errorf("self-parent: status %d body %v, want 400", code, body)
	}
	// Indirect cycle: leaf→mid is live; mid→leaf closes mid→leaf→mid.
	code, body = a.do(t, "PUT", "/api/v1/projects/1586-mid", map[string]interface{}{"parent": "1586-leaf"})
	if code != 400 {
		t.Errorf("indirect cycle: status %d body %v, want 400", code, body)
	}
	// A rejected write must not have landed.
	code, body = a.do(t, "GET", "/api/v1/projects/1586-mid", nil)
	if code != 200 {
		t.Fatalf("GET after rejected PUT: status %d", code)
	}
	if mid := body["project"].(map[string]interface{}); mid["parent"] != "" {
		t.Errorf("rejected PUT changed state: parent %v, want \"\"", mid["parent"])
	}

	// Clearing works: leaf is a primary again.
	code, body = a.do(t, "PUT", "/api/v1/projects/1586-leaf", map[string]interface{}{"parent": ""})
	if code != 200 {
		t.Fatalf("clear parent: status %d body %v", code, body)
	}
	if body["parent"] != "" {
		t.Errorf("after clear parent = %v, want \"\"", body["parent"])
	}

	// A dangling parent (no such lane) is tolerated at the API too — the
	// same contract the storage layer enforces.
	code, body = a.do(t, "PUT", "/api/v1/projects/1586-leaf", map[string]interface{}{"parent": "1586-purged-lane"})
	if code != 200 || body["parent"] != "1586-purged-lane" {
		t.Errorf("dangling parent: status %d parent %v, want 200 / 1586-purged-lane", code, body["parent"])
	}
}
