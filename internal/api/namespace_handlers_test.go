package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func createTestNamespace(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	ns := &database.Namespace{
		ID:       id,
		Weight:   20,
		Reserved: 5,
		HardCap:  30,
		Enabled:  true,
	}
	if err := database.CreateNamespace(context.Background(), db, ns); err != nil {
		t.Fatalf("CreateNamespace %s: %v", id, err)
	}
}

// --- create namespace ---

func TestCreateNamespace(t *testing.T) {
	a := newAPITestServer(t)
	body := map[string]interface{}{
		"id":       "coding-hermes",
		"weight":   30,
		"reserved": 10,
		"hard_cap": 50,
		"enabled":  true,
	}
	status, resp := a.do(t, "POST", "/api/v1/namespaces", body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", status, resp)
	}
	if resp["id"] != "coding-hermes" {
		t.Errorf("id = %v, want coding-hermes", resp["id"])
	}
	if w, ok := resp["weight"].(float64); !ok || int(w) != 30 {
		t.Errorf("weight = %v, want 30", resp["weight"])
	}
	// Verify it was persisted.
	ns, err := database.GetNamespace(context.Background(), a.db, "coding-hermes")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.Weight != 30 {
		t.Errorf("persisted weight = %d, want 30", ns.Weight)
	}
}

func TestCreateNamespaceDuplicate(t *testing.T) {
	a := newAPITestServer(t)
	body := map[string]interface{}{
		"id":     "dup-ns",
		"weight": 20,
	}
	status, _ := a.do(t, "POST", "/api/v1/namespaces", body)
	if status != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", status)
	}
	status, _ = a.do(t, "POST", "/api/v1/namespaces", body)
	if status != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", status)
	}
}

func TestCreateNamespaceInvalid(t *testing.T) {
	a := newAPITestServer(t)
	// Missing id.
	status, resp := a.do(t, "POST", "/api/v1/namespaces", map[string]interface{}{
		"weight": 20,
	})
	if status != http.StatusBadRequest {
		t.Errorf("missing id: status = %d, want 400: %v", status, resp)
	}
	// Weight <= 0.
	status, resp = a.do(t, "POST", "/api/v1/namespaces", map[string]interface{}{
		"id":     "zero-weight",
		"weight": 0,
	})
	if status != http.StatusBadRequest {
		t.Errorf("zero weight: status = %d, want 400: %v", status, resp)
	}
}

// --- list namespaces ---

func TestListNamespaces(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "alpha")
	createTestNamespace(t, a.db, "beta")

	status, body := a.do(t, "GET", "/api/v1/namespaces", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	list, ok := body["namespaces"].([]interface{})
	if !ok {
		t.Fatalf("namespaces field not an array: %T", body["namespaces"])
	}
	if len(list) != 2 {
		t.Errorf("got %d namespaces, want 2", len(list))
	}
}

func TestListNamespacesEmpty(t *testing.T) {
	a := newAPITestServer(t)
	status, body := a.do(t, "GET", "/api/v1/namespaces", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	list, ok := body["namespaces"].([]interface{})
	if !ok {
		t.Fatalf("namespaces field not an array: %T", body["namespaces"])
	}
	if len(list) != 0 {
		t.Errorf("got %d namespaces, want 0", len(list))
	}
}

// --- get namespace ---

func TestGetNamespace(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "prod")

	status, body := a.do(t, "GET", "/api/v1/namespaces/prod", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["id"] != "prod" {
		t.Errorf("id = %v, want prod", body["id"])
	}
	if w, ok := body["weight"].(float64); !ok || int(w) != 20 {
		t.Errorf("weight = %v, want 20", body["weight"])
	}
}

func TestGetNamespaceNotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "GET", "/api/v1/namespaces/nonexistent", nil)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- update namespace ---

func TestUpdateNamespace(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "dev")

	status, _ := a.do(t, "PUT", "/api/v1/namespaces/dev", map[string]interface{}{
		"weight": 50,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	ns, err := database.GetNamespace(context.Background(), a.db, "dev")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.Weight != 50 {
		t.Errorf("weight = %d, want 50", ns.Weight)
	}
}

func TestUpdateNamespaceNotFound(t *testing.T) {
	a := newAPITestServer(t)
	status, _ := a.do(t, "PUT", "/api/v1/namespaces/nonexistent", map[string]interface{}{
		"weight": 50,
	})
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- list namespace projects ---

func TestListNamespaceProjects(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "team-a")

	nsID := "team-a"
	mustCreateAPITestProject(t, a.db, "proj-a")
	if err := database.UpdateProject(context.Background(), a.db, "proj-a", database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	status, body := a.do(t, "GET", "/api/v1/namespaces/team-a/projects", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["namespace_id"] != "team-a" {
		t.Errorf("namespace_id = %v, want team-a", body["namespace_id"])
	}
	projects, ok := body["projects"].([]interface{})
	if !ok {
		t.Fatalf("projects field not an array: %T", body["projects"])
	}
	if len(projects) != 1 {
		t.Fatalf("got %d projects, want 1", len(projects))
	}
}

// --- move project to namespace ---

func TestMoveProjectToNamespace(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "target-ns")
	mustCreateAPITestProject(t, a.db, "moveme")

	status, _ := a.do(t, "POST", "/api/v1/namespaces/target-ns/move", map[string]interface{}{
		"project": "moveme",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	p, err := database.GetProject(context.Background(), a.db, "moveme")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.NamespaceID == nil || *p.NamespaceID != "target-ns" {
		t.Errorf("namespace_id = %v, want target-ns", p.NamespaceID)
	}
}

// --- delete namespace (SCHED-GAP-097) ---

// TestDeleteNamespaceNoConfirm verifies DELETE without the confirm=true
// query param is refused with 400 and an actionable message, and the
// namespace is left untouched.
func TestDeleteNamespaceNoConfirm(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "doomed")

	status, resp := a.do(t, "DELETE", "/api/v1/namespaces/doomed", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}
	// Namespace still present and untouched.
	ns, err := database.GetNamespace(context.Background(), a.db, "doomed")
	if err != nil {
		t.Fatalf("GetNamespace after refused delete: %v", err)
	}
	if !ns.Enabled {
		t.Error("namespace disabled by DELETE without confirm")
	}
}

// TestDeleteNamespacePurgeWithoutConfirm verifies purge has its OWN confirm
// requirement: ?purge=true without confirm=true is refused with 400 and the
// row is left untouched.
func TestDeleteNamespacePurgeWithoutConfirm(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "purgedummy")

	status, resp := a.do(t, "DELETE", "/api/v1/namespaces/purgedummy?purge=true", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (purge without confirm): %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}
	if _, err := database.GetNamespace(context.Background(), a.db, "purgedummy"); err != nil {
		t.Errorf("GetNamespace after refused purge: %v", err)
	}
}

// TestDeleteNamespaceEnabledMemberRefused verifies a namespace that still has
// an ENABLED member project is refused with 409 even with confirm=true, the
// error body lists the member project name, and nothing is changed. The
// confirm gate runs FIRST: a bare DELETE of the same namespace is 400, not
// 409.
func TestDeleteNamespaceEnabledMemberRefused(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "live-ns")
	mustCreateAPITestProject(t, a.db, "live-member") // enabled
	nsID := "live-ns"
	if err := database.UpdateProject(context.Background(), a.db, "live-member", database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}

	status, resp := a.do(t, "DELETE", "/api/v1/namespaces/live-ns", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (confirm checked before enabled-member guard): %v", status, resp)
	}
	msg, _ := resp["error"].(string)
	if !strings.Contains(msg, "confirm=true") {
		t.Errorf("error = %q, want mention of confirm=true", msg)
	}

	status, resp = a.do(t, "DELETE", "/api/v1/namespaces/live-ns?confirm=true", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %v", status, resp)
	}
	msg, _ = resp["error"].(string)
	if !strings.Contains(msg, "live-member") {
		t.Errorf("error = %q, want it to list the enabled member project name", msg)
	}
	if !strings.Contains(msg, "pause or move them first") {
		t.Errorf("error = %q, want the pause-or-move guard message", msg)
	}
	// Namespace row intact and still enabled.
	ns, err := database.GetNamespace(context.Background(), a.db, "live-ns")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if !ns.Enabled {
		t.Error("namespace disabled by refused delete")
	}
	// Member still assigned and enabled.
	p, err := database.GetProject(context.Background(), a.db, "live-member")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.NamespaceID == nil || *p.NamespaceID != "live-ns" {
		t.Errorf("member namespace_id = %v, want live-ns (must stay assigned)", p.NamespaceID)
	}
	if !p.Enabled {
		t.Error("member disabled by refused delete")
	}
}

// TestDeleteNamespaceSoftDeleteUnassignsMember verifies the full guard flow:
// after the member is paused, confirm=true soft-deletes the namespace (200,
// status=deleted, row retained with enabled=false) and the member's
// namespace_id becomes NULL — the retained row never fires the FK ON DELETE
// SET NULL, so the handler must unassign members explicitly.
func TestDeleteNamespaceSoftDeleteUnassignsMember(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "retire-ns")
	mustCreateAPITestProject(t, a.db, "member-a") // enabled
	nsID := "retire-ns"
	if err := database.UpdateProject(context.Background(), a.db, "member-a", database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		t.Fatalf("assign member: %v", err)
	}
	// Pause the member before the delete (the guard's required pre-step).
	if err := database.UpdateProject(context.Background(), a.db, "member-a", database.ProjectUpdates{Enabled: database.BoolPtr(false)}); err != nil {
		t.Fatalf("pause member: %v", err)
	}

	status, resp := a.do(t, "DELETE", "/api/v1/namespaces/retire-ns?confirm=true", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "deleted" {
		t.Errorf("status field = %v, want deleted", resp["status"])
	}
	if resp["namespace"] != "retire-ns" {
		t.Errorf("namespace field = %v, want retire-ns", resp["namespace"])
	}
	// Soft delete: row retained with enabled=false (restorable via PUT).
	ns, err := database.GetNamespace(context.Background(), a.db, "retire-ns")
	if err != nil {
		t.Fatalf("GetNamespace after delete: %v", err)
	}
	if ns.Enabled {
		t.Error("namespace still enabled after soft delete")
	}
	// Member unassigned: namespace_id NULL.
	p, err := database.GetProject(context.Background(), a.db, "member-a")
	if err != nil {
		t.Fatalf("GetProject after delete: %v", err)
	}
	if p.NamespaceID != nil {
		t.Errorf("member namespace_id after delete = %v, want NULL", p.NamespaceID)
	}
}

// TestDeleteNamespacePurgeSuccess verifies confirm=true&purge=true
// hard-deletes: 200 with status=purged, the row disappears from the DB, a
// historical namespace_tick survives (its FK is NO ACTION — the purge must
// disable FK enforcement for the DELETE), the member's namespace_id is NULL,
// and a purge audit event is logged.
func TestDeleteNamespacePurgeSuccess(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "purge-ns")
	mustCreateAPITestProject(t, a.db, "member-b") // enabled
	nsID := "purge-ns"
	if err := database.UpdateProject(context.Background(), a.db, "member-b", database.ProjectUpdates{NamespaceID: &nsID}); err != nil {
		t.Fatalf("assign member: %v", err)
	}
	// Pause the member — the guard requires no ENABLED members.
	if err := database.UpdateProject(context.Background(), a.db, "member-b", database.ProjectUpdates{Enabled: database.BoolPtr(false)}); err != nil {
		t.Fatalf("pause member: %v", err)
	}
	// A historical namespace_tick referencing the namespace — blocks the
	// DELETE while FK enforcement is on.
	if err := database.InsertNamespaceTick(context.Background(), a.db, &database.NamespaceTick{
		TickGroup:   "2026-09-02-00-00-00",
		NamespaceID: "purge-ns",
		Allocated:   5,
		Used:        2,
		JobCount:    1,
	}); err != nil {
		t.Fatalf("InsertNamespaceTick: %v", err)
	}

	status, resp := a.do(t, "DELETE", "/api/v1/namespaces/purge-ns?confirm=true&purge=true", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", status, resp)
	}
	if resp["status"] != "purged" {
		t.Errorf("status field = %v, want purged", resp["status"])
	}
	if resp["namespace"] != "purge-ns" {
		t.Errorf("namespace field = %v, want purge-ns", resp["namespace"])
	}
	// Row gone from the namespaces table.
	if _, err := database.GetNamespace(context.Background(), a.db, "purge-ns"); err == nil {
		t.Error("purged namespace still present in DB")
	}
	// Member project kept but unassigned: namespace_id NULL.
	p, err := database.GetProject(context.Background(), a.db, "member-b")
	if err != nil {
		t.Fatalf("GetProject after purge: %v", err)
	}
	if p.NamespaceID != nil {
		t.Errorf("member namespace_id after purge = %v, want NULL", p.NamespaceID)
	}
	// Historical namespace_tick retained.
	var n int
	if err := a.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM namespace_ticks WHERE namespace_id = 'purge-ns'`).Scan(&n); err != nil {
		t.Fatalf("count namespace_ticks: %v", err)
	}
	if n != 1 {
		t.Errorf("namespace_ticks after purge = %d, want 1 (historical ticks retained)", n)
	}
	// Purge audit event logged by the handler (component api, INFO).
	evs, err := database.ListEvents(context.Background(), a.db, "", "api", 10, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Message == "namespace purged (hard delete): purge-ns" {
			found = true
			break
		}
	}
	if !found {
		t.Error("no purge audit event with message 'namespace purged (hard delete): purge-ns'")
	}
}

// TestDeleteNamespaceNotFound verifies an unknown namespace id maps to 404
// on ANY DELETE variant — bare, confirm=true, and confirm=true&purge=true.
func TestDeleteNamespaceNotFound(t *testing.T) {
	a := newAPITestServer(t)
	for _, path := range []string{
		"/api/v1/namespaces/no-such-ns",
		"/api/v1/namespaces/no-such-ns?confirm=true",
		"/api/v1/namespaces/no-such-ns?confirm=true&purge=true",
	} {
		status, resp := a.do(t, "DELETE", path, nil)
		if status != http.StatusNotFound {
			t.Errorf("DELETE %s: status = %d, want 404: %v", path, status, resp)
		}
		if errMsg, _ := resp["error"].(string); !strings.Contains(errMsg, "not found") {
			t.Errorf("DELETE %s: error = %v, want containing 'not found'", path, resp["error"])
		}
	}
}

func TestDeleteNamespaceWrongMethod(t *testing.T) {
	a := newAPITestServer(t)
	createTestNamespace(t, a.db, "keep-me")
	// PATCH is not a supported method on /namespaces/{id} → 405.
	status, resp := a.do(t, "PATCH", "/api/v1/namespaces/keep-me", map[string]interface{}{
		"weight": 40,
	})
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405: %v", status, resp)
	}
	if errMsg, _ := resp["error"].(string); errMsg != "GET, PUT, or DELETE only" {
		t.Errorf("error = %v, want 'GET, PUT, or DELETE only'", resp["error"])
	}
}
