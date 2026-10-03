package config

import (
	"context"
	"database/sql"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1712 durability: the endpoint tiers follow the GatewayKey
// conditional-pin law — the PRESENCE of the key in fleet.toml is the
// operator's signal.
//
//   - a lane entry carrying gateway_url re-pins it over DB drift;
//   - a lane entry with NO gateway_url never clears an API-assigned endpoint;
//   - the namespace tier (gateway_url and gateway_key) behaves identically.
//
// The law matters because the endpoint is what makes a lane ADDRESSABLE: a
// restart silently dropping a lane back onto the shared gateway would be a
// routing regression with no error anywhere.

func gap1712DB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return db, func() { db.Close() }
}

// TestSCHEDGAP1712_LaneEndpointPinsOverDBDrift pins the lane tier: the file
// value re-wins over drift, and the pin is scoped to the keyed field only.
func TestSCHEDGAP1712_LaneEndpointPinsOverDBDrift(t *testing.T) {
	db, closeDB := gap1712DB(t)
	defer closeDB()
	ctx := context.Background()

	cfg := &FleetConfig{Projects: []ProjectDef{{
		Name:       "lane1712-pin",
		RepoURL:    "git@example.invalid:x.git",
		Workdir:    "/tmp/lane1712-pin",
		GatewayURL: "http://lane-a:8642",
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (create): %v", err)
	}
	p, err := database.GetProject(ctx, db, "lane1712-pin")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.GatewayURL != "http://lane-a:8642" {
		t.Fatalf("gateway_url not stored at creation: got %q want http://lane-a:8642", p.GatewayURL)
	}

	// DB drift on the pinned field + a DB-only value on a field the entry
	// does not carry (Prompt), so the scoping half is observable too.
	drift := "http://drifted:1111"
	dbOnlyPrompt := "db-only prompt"
	if err := database.UpdateProject(ctx, db, "lane1712-pin", database.ProjectUpdates{
		GatewayURL: &drift, Prompt: &dbOnlyPrompt,
	}); err != nil {
		t.Fatalf("UpdateProject (drift): %v", err)
	}

	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	p, _ = database.GetProject(ctx, db, "lane1712-pin")
	if p.GatewayURL != "http://lane-a:8642" {
		t.Errorf("re-pin FAIL: gateway_url = %q, want the fleet.toml value http://lane-a:8642", p.GatewayURL)
	}
	if p.Prompt != dbOnlyPrompt {
		t.Errorf("pin leaked beyond its scope: prompt = %q, want the DB-only %q preserved", p.Prompt, dbOnlyPrompt)
	}
}

// TestSCHEDGAP1712_KeylessLaneEntryPreservesAPIAssignedEndpoint pins the
// GatewayKey-conditional half: an entry with no gateway_url must never clear
// an endpoint assigned through the API.
func TestSCHEDGAP1712_KeylessLaneEntryPreservesAPIAssignedEndpoint(t *testing.T) {
	db, closeDB := gap1712DB(t)
	defer closeDB()
	ctx := context.Background()

	if err := ApplyFleetConfig(ctx, db, &FleetConfig{Projects: []ProjectDef{{
		Name: "lane1712-keyless", RepoURL: "git@example.invalid:y.git", Workdir: "/tmp/lane1712-keyless",
	}}}); err != nil {
		t.Fatalf("ApplyFleetConfig (create keyless): %v", err)
	}
	apiURL := "http://assigned-via-api:8642"
	if err := database.UpdateProject(ctx, db, "lane1712-keyless", database.ProjectUpdates{GatewayURL: &apiURL}); err != nil {
		t.Fatalf("UpdateProject (API assign): %v", err)
	}

	// The SAME keyless entry applied again — a restart.
	if err := ApplyFleetConfig(ctx, db, &FleetConfig{Projects: []ProjectDef{{
		Name: "lane1712-keyless", RepoURL: "git@example.invalid:y.git", Workdir: "/tmp/lane1712-keyless",
	}}}); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}
	p, _ := database.GetProject(ctx, db, "lane1712-keyless")
	if p.GatewayURL != apiURL {
		t.Errorf("keyless restart CLEARED the endpoint: gateway_url = %q, want the API value %q preserved", p.GatewayURL, apiURL)
	}
}

// TestSCHEDGAP1712_NamespaceEndpointRepinsAndIsKeylessSafe pins the namespace
// tier for both fields.
func TestSCHEDGAP1712_NamespaceEndpointRepinsAndIsKeylessSafe(t *testing.T) {
	db, closeDB := gap1712DB(t)
	defer closeDB()
	ctx := context.Background()

	cfg := &FleetConfig{Namespaces: []NamespaceDef{{
		ID: "ns1712pin", GatewayURL: "http://ns-a:8642", GatewayKey: "fk-ns-a",
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (create): %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns1712pin")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.GatewayURL != "http://ns-a:8642" || ns.GatewayKey != "fk-ns-a" {
		t.Fatalf("namespace endpoint not stored at creation: got (%q, %q)", ns.GatewayURL, ns.GatewayKey)
	}

	driftURL, driftKey := "http://ns-drift:1111", "fk-drift"
	if err := database.UpdateNamespace(ctx, db, "ns1712pin", database.NamespacePatch{
		GatewayURL: &driftURL, GatewayKey: &driftKey,
	}); err != nil {
		t.Fatalf("UpdateNamespace (drift): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	ns, _ = database.GetNamespace(ctx, db, "ns1712pin")
	if ns.GatewayURL != "http://ns-a:8642" || ns.GatewayKey != "fk-ns-a" {
		t.Errorf("namespace re-pin FAIL: got (%q, %q), want the fleet.toml values", ns.GatewayURL, ns.GatewayKey)
	}

	// Keyless entry: neither field is touched.
	apiURL, apiKey := "http://ns-api:9000", "fk-api"
	if err := database.UpdateNamespace(ctx, db, "ns1712pin", database.NamespacePatch{
		GatewayURL: &apiURL, GatewayKey: &apiKey,
	}); err != nil {
		t.Fatalf("UpdateNamespace (API assign): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, &FleetConfig{Namespaces: []NamespaceDef{{ID: "ns1712pin"}}}); err != nil {
		t.Fatalf("ApplyFleetConfig (keyless restart): %v", err)
	}
	ns, _ = database.GetNamespace(ctx, db, "ns1712pin")
	if ns.GatewayURL != apiURL || ns.GatewayKey != apiKey {
		t.Errorf("keyless namespace restart CLEARED the endpoint: got (%q, %q), want (%q, %q) preserved", ns.GatewayURL, ns.GatewayKey, apiURL, apiKey)
	}
}
