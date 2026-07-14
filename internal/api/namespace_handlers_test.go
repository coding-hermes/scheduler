package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
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
