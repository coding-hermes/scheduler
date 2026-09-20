package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// REG-002 — "parked is not abandoned"
//
// An operator-set cooldown is a DELIBERATE park, not a failure. It must
// survive a store close/reopen, must never be escalated below the value in
// force when the project was enabled, must not escalate at all while the
// project is disabled, and must reset cleanly when the project is re-enabled.
//
// Evidence this pins: on 2026-09-02 `muster` and `temple-runner` were parked
// to a 604800s cooldown through PUT /api/v1/projects/<name>, and the parked
// value was verified to survive two daemon restarts. A daemon restart at the
// storage layer is exactly: close the *sql.DB, then InitDB() the SAME file
// again (which replays PRAGMAs + Migrate before serving). If a reopen ever
// re-derived a column default instead of reading the stored row, the park
// would silently evaporate on every restart — the project would quietly come
// back at a fleet-baseline cadence while the operator believed it parked.

// reg002ParkRow is the full park shape: every adaptive/cooldown column whose
// value a reopen must preserve, in one comparable struct so a drift in ANY of
// them is a single clear failure.
type reg002ParkRow struct {
	cooldown    int
	floor       int
	ceiling     int
	threshold   int
	streak      int
	rowsSeen    int
	openSeen    int
	enabled     int
	adaptive    int
	disabledBy  string
	migrationNo int
}

// reg002OpenStore opens (or reopens) the scheduler store at path using the
// production entry point — InitDB applies the WAL/FK PRAGMAs and Migrate, so a
// reopen here is the storage half of a daemon restart. Registered for cleanup
// so the test never leaks a file handle; tests that close early are still fine
// (Close is idempotent enough for our purposes and the deferred call ignores
// the second error).
func reg002OpenStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// reg002ReadPark reads the park shape for one project.
func reg002ReadPark(t *testing.T, db *sql.DB, name string) reg002ParkRow {
	t.Helper()
	var r reg002ParkRow
	err := db.QueryRow(`SELECT cooldown_s, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_threshold, no_progress_ticks, board_rows_seen,
	       COALESCE(board_open_seen, -1), enabled, COALESCE(adaptive_cooldown, 0),
	       COALESCE(disabled_by, '')
	FROM projects WHERE name = ?`, name).
		Scan(&r.cooldown, &r.floor, &r.ceiling, &r.threshold, &r.streak,
			&r.rowsSeen, &r.openSeen, &r.enabled, &r.adaptive, &r.disabledBy)
	if err != nil {
		t.Fatalf("read park row %q: %v", name, err)
	}
	v, err := MigrationVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	r.migrationNo = v
	return r
}

// reg002AssertAPIPutUsesUpdateProject pins the write-path identity this test
// relies on: the API's PUT /api/v1/projects/{name} handler applies the decoded
// ProjectUpdates through database.UpdateProject. If the API is ever re-plumbed
// onto a different write path (a direct UPDATE, a repository method), this
// test's claim that "the value written here is the value the API PUT writes"
// stops being true and must fail loudly rather than rot into a comfortable
// fiction.
func reg002AssertAPIPutUsesUpdateProject(t *testing.T) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/database/x_test.go -> repo root
	handlerPath := filepath.Join(repoRoot, "internal", "api", "server_projects.go")
	b, err := os.ReadFile(handlerPath)
	if err != nil {
		t.Fatalf("read api PUT handler %s: %v", handlerPath, err)
	}
	src := string(b)
	const want = "database.UpdateProject(ctx, s.db, name, updates)"
	if !strings.Contains(src, want) {
		t.Errorf("api PUT handler no longer applies updates through %q — this test drives that path and must be re-aimed at the new one", want)
	}
	if !strings.Contains(src, "json.NewDecoder(r.Body).Decode(&updates)") {
		t.Errorf("api PUT handler no longer decodes the request body into database.ProjectUpdates — the JSON park shape asserted here is no longer the API's")
	}
}

// TestREG002_ParkedCooldownSurvivesStoreReopen parks a project the way the
// 2026-09-02 muster/temple-runner parks did (PUT cooldown_s = 604800) through
// the exact update path the API PUT uses, then closes the store and reopens
// it against the SAME file twice (the storage half of two daemon restarts).
// The parked value — and every adaptive column beside it — must read back
// unchanged: a reopen must never re-derive a default over an operator's park.
func TestREG002_ParkedCooldownSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "scheduler.db")

	reg002AssertAPIPutUsesUpdateProject(t)

	// Decode the park from the JSON body shape the API accepts, so the value
	// this test writes is the value an operator's PUT delivers — not a
	// hand-built struct that could diverge from the wire contract.
	var updates ProjectUpdates
	if err := json.Unmarshal([]byte(`{"cooldown_s": 604800}`), &updates); err != nil {
		t.Fatalf("decode PUT body {\"cooldown_s\": 604800}: %v", err)
	}
	if updates.CooldownS == nil || *updates.CooldownS != 604800 {
		t.Fatalf("PUT body decoded cooldown_s = %v, want 604800", updates.CooldownS)
	}

	const name = "reg002-parked"

	db := reg002OpenStore(t, dbPath)
	p := sampleProject(name)
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject(%q): %v", name, err)
	}
	if err := UpdateProject(ctx, db, name, updates); err != nil {
		t.Fatalf("UpdateProject(park): %v", err)
	}
	// The 09-02 parks were accompanied by a pause: the row stays parked while
	// disabled, so the reopen must preserve enabled=0 too — a restart that
	// silently re-enabled a paused project would abandon it to the packer.
	if err := UpdateProject(ctx, db, name, ProjectUpdates{
		Enabled:        BoolPtr(false),
		DisabledBy:     strPtrReg002("api-pause"),
		DisabledReason: strPtrReg002("REG-002 park: deliberate operator pause"),
	}); err != nil {
		t.Fatalf("UpdateProject(pause): %v", err)
	}

	parked := reg002ReadPark(t, db, name)
	if parked.cooldown != 604800 {
		t.Fatalf("park did not land before the reopen: cooldown_s = %d, want 604800", parked.cooldown)
	}
	if parked.enabled != 0 {
		t.Fatalf("pause did not land before the reopen: enabled = %d, want 0", parked.enabled)
	}

	// Restart 1: close the store the daemon served from.
	if err := db.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	db2 := reg002OpenStore(t, dbPath)
	afterFirst := reg002ReadPark(t, db2, name)
	if afterFirst != parked {
		t.Errorf("park changed across the first store close/reopen (\"parked is not abandoned\": an operator park must survive a daemon restart)\n before: %+v\n after:  %+v", parked, afterFirst)
	}

	// Restart 2: the 09-02 parks survived two restarts — pin the second too.
	if err := db2.Close(); err != nil {
		t.Fatalf("close store before second reopen: %v", err)
	}
	db3 := reg002OpenStore(t, dbPath)
	afterSecond := reg002ReadPark(t, db3, name)
	if afterSecond != parked {
		t.Errorf("park changed across the second store close/reopen\n before: %+v\n after:  %+v", parked, afterSecond)
	}
	if afterSecond.cooldown != 604800 {
		t.Errorf("cooldown_s = %d after two reopens, want 604800 — the operator's park was overwritten by a re-derived default", afterSecond.cooldown)
	}
}

func strPtrReg002(s string) *string { return &s }
