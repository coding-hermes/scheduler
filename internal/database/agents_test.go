package database

// SCHED-GAP-1665 — the agent registry store: CRUD, normalization, and the
// property the whole feature exists for: a registration SURVIVES A RESTART.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentsCRUDPersistsAcrossRestart drives register → read → refresh → list
// → unregister on a FILE-backed database, then reopens the file and asserts the
// row is still there. The restart half is the acceptance criterion ("agents
// persist in the database, survive restarts") — an in-memory DB cannot prove it.
func TestAgentsCRUDPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler.db")
	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	ctx := context.Background()

	first := &Agent{
		Name:         "helix-agent",
		Capabilities: " control , dispatch ,Control ",
		Endpoint:     "http://10.0.0.7:8767",
	}
	created, err := UpsertAgent(ctx, db, first)
	if err != nil {
		t.Fatalf("UpsertAgent(create): %v", err)
	}
	if !created {
		t.Error("created = false, want true on the first registration")
	}
	if first.Capabilities != "control, dispatch" {
		t.Errorf("capabilities = %q, want normalized %q", first.Capabilities, "control, dispatch")
	}
	if first.RegisteredAt == "" || first.UpdatedAt == "" {
		t.Errorf("bookkeeping stamps missing: registered_at=%q updated_at=%q", first.RegisteredAt, first.UpdatedAt)
	}
	registeredAt := first.RegisteredAt

	got, err := GetAgent(ctx, db, "helix-agent")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Endpoint != "http://10.0.0.7:8767" || got.Capabilities != "control, dispatch" {
		t.Errorf("GetAgent = %+v, want the registered capabilities + endpoint", got)
	}

	// Re-registering refreshes the advertised facts and PRESERVES the
	// registration instant (an inventory row's history is not rewritten by a
	// refresh).
	refresh := &Agent{Name: "helix-agent", Capabilities: "dispatch", Endpoint: ""}
	created, err = UpsertAgent(ctx, db, refresh)
	if err != nil {
		t.Fatalf("UpsertAgent(refresh): %v", err)
	}
	if created {
		t.Error("created = true on a re-registration, want false (update arm)")
	}
	if refresh.RegisteredAt != registeredAt {
		t.Errorf("registered_at = %q after refresh, want the original %q", refresh.RegisteredAt, registeredAt)
	}
	got, err = GetAgent(ctx, db, "helix-agent")
	if err != nil {
		t.Fatalf("GetAgent after refresh: %v", err)
	}
	if got.Capabilities != "dispatch" || got.Endpoint != "" {
		t.Errorf("refreshed row = %+v, want capabilities=dispatch endpoint=\"\" (cleared)", got)
	}

	// A second agent, so the list ordering and the capability filter have
	// something to discriminate.
	if _, err := UpsertAgent(ctx, db, &Agent{Name: "bunker-alpha", Capabilities: "control"}); err != nil {
		t.Fatalf("UpsertAgent(second): %v", err)
	}
	all, err := ListAgents(ctx, db, "")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(all) != 2 || all[0].Name != "bunker-alpha" || all[1].Name != "helix-agent" {
		t.Errorf("ListAgents = %+v, want [bunker-alpha helix-agent] ordered by name", all)
	}
	dispatchOnly, err := ListAgents(ctx, db, "DISPATCH")
	if err != nil {
		t.Fatalf("ListAgents(dispatch): %v", err)
	}
	if len(dispatchOnly) != 1 || dispatchOnly[0].Name != "helix-agent" {
		t.Errorf("ListAgents(capability=dispatch) = %+v, want only helix-agent (case-insensitive token match)", dispatchOnly)
	}

	// RESTART: close the handle and reopen the same file. The row must still
	// be there — this is the AC the in-memory tests cannot cover.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	db2, err := InitDB(path)
	if err != nil {
		t.Fatalf("reopen InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	reloaded, err := GetAgent(ctx, db2, "helix-agent")
	if err != nil {
		t.Fatalf("GetAgent after restart: %v", err)
	}
	if reloaded.Capabilities != "dispatch" || reloaded.RegisteredAt != registeredAt {
		t.Errorf("after restart = %+v, want the refreshed capabilities and the original registered_at", reloaded)
	}

	// Unregister: the row goes, and a second delete is a named not-found —
	// never a silent success.
	if err := DeleteAgent(ctx, db2, "helix-agent"); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if _, err := GetAgent(ctx, db2, "helix-agent"); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("GetAgent after delete error = %v, want ErrAgentNotFound", err)
	}
	if err := DeleteAgent(ctx, db2, "helix-agent"); !errors.Is(err, ErrAgentNotFound) {
		t.Errorf("second DeleteAgent error = %v, want ErrAgentNotFound", err)
	}
}

// TestMigrationLandsAgentsTable pins the schema migration the store depends on.
func TestMigrationLandsAgentsTable(t *testing.T) {
	if latestMigration < 71 {
		t.Fatalf("latestMigration = %d, want >= 71 (SCHED-GAP-1665 lands the agents table as v71)", latestMigration)
	}
	db, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='agents'`).Scan(&name)
	if err != nil {
		t.Fatalf("agents table missing after Migrate: %v", err)
	}
	// The migration is recorded, so a second boot is a no-op.
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 71`).Scan(&applied); err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("migration 71 recorded %d times, want 1", applied)
	}
}

// TestValidateAgentName is the write-path charset gate: a name is interpolated
// into a relay path and into audit rows, so anything that cannot be a bus
// identity must be refused.
func TestValidateAgentName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"plain", "helix", true},
		{"dotted", "bunker-helix.agent_1", true},
		{"mixed case (fleet ids are)", "karaHermes-mde-7840hs", true},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"leading space", " helix", false},
		{"path separator", "box/agent", false},
		{"colon", "box:8767", false},
		{"space inside", "agent one", false},
		{"at sign", "agent@box", false},
		{"too long", strings.Repeat("a", MaxAgentNameLen+1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgentName(tc.input)
			if tc.ok && err != nil {
				t.Errorf("ValidateAgentName(%q) = %v, want nil", tc.input, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateAgentName(%q) = nil, want a refusal", tc.input)
			}
		})
	}
}

// TestValidateAgentEndpoint — "" inherits the scheduler's relay; anything else
// must be an absolute http(s) URL with a host.
func TestValidateAgentEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"empty inherits", "", true},
		{"http", "http://10.0.0.7:8767", true},
		{"https with path", "https://relay.example.net/bus", true},
		{"bare host:port", "10.0.0.7:8767", false},
		{"relative path", "/relay", false},
		{"no host", "http://", false},
		{"wrong scheme", "ftp://relay", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAgentEndpoint(tc.input)
			if tc.ok && err != nil {
				t.Errorf("ValidateAgentEndpoint(%q) = %v, want nil", tc.input, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ValidateAgentEndpoint(%q) = nil, want a refusal", tc.input)
			}
		})
	}
}

// TestAgentHasCapability — token-wise, case-insensitive; a prefix is not a
// capability ("dispatch" must not match "dispatcher").
func TestAgentHasCapability(t *testing.T) {
	const caps = "control, dispatch, query"
	cases := []struct {
		want string
		ok   bool
	}{
		{"dispatch", true},
		{"DISPATCH", true},
		{" Control ", true},
		{"dispatcher", false},
		{"disp", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := AgentHasCapability(caps, tc.want); got != tc.ok {
			t.Errorf("AgentHasCapability(%q, %q) = %v, want %v", caps, tc.want, got, tc.ok)
		}
	}
}
