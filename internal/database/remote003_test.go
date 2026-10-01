package database

import (
	"context"
	"testing"
	"time"
)

// REMOTE-003 §1/§2 acceptance tests: the identity migration + backfill, the
// per-row scheduler_id stamp on every insert path, and the local peer
// registry with the stale/last_contact rendering law.

// TestMigrationV56SchedulerIDColumns proves the v56 migration lands the
// scheduler_id column on projects, ticks AND events on a FRESH database —
// and that latestMigration carries it (the schedgap215 test family's
// floor-pin shape: a silent migration-list edit must fail here).
func TestMigrationV56SchedulerIDColumns(t *testing.T) {
	if latestMigration < 57 {
		t.Fatalf("latestMigration = %d, want >= 57 (REMOTE-003 lands v56 scheduler_id + v57 peers)", latestMigration)
	}
	db := newTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"projects", "ticks", "events"} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'scheduler_id'`, table,
		).Scan(&count); err != nil {
			t.Fatalf("pragma_table_info(%s): %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s: scheduler_id column missing after migration (pragma rows=%d)", table, count)
		}
	}
	// The v56 ledger row must be recorded with the REMOTE-003 desc.
	var desc string
	if err := db.QueryRowContext(ctx,
		`SELECT desc FROM migrations WHERE version = 56`).Scan(&desc); err != nil {
		t.Fatalf("migration v56 ledger row missing: %v", err)
	}
	if !contains(desc, "REMOTE-003") {
		t.Errorf("v56 desc = %q, want it to name REMOTE-003", desc)
	}
	// The peers table (v57) must exist alongside.
	var peersTable int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='peers'`).Scan(&peersTable); err != nil {
		t.Fatalf("sqlite_master peers probe: %v", err)
	}
	if peersTable != 1 {
		t.Error("peers table missing after migration (v57, REMOTE-003 §2)")
	}
}

// TestMigrationBackfillsExistingRowsWithLocalID is acceptance #2's backfill
// half: pre-existing rows (written before identity existed, scheduler_id=”)
// are claimed for the LOCAL id by BackfillSchedulerID at first boot.
func TestMigrationBackfillsExistingRowsWithLocalID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Seed rows through the real write paths (they stamp "" because no
	// identity has been installed in this test process), simulating a
	// database written by a pre-REMOTE-003 daemon.
	p := &Project{Weight: 10, Priority: 5, Name: "legacy-lane", RepoURL: "https://example.com/legacy", Workdir: "/tmp/legacy-lane"}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := &Tick{ID: NextTickID(ctx, "legacy-lane"), ProjectName: "legacy-lane"}
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := LogEvent(ctx, db, &Event{Severity: SeverityInfo, Component: "test", Message: "legacy row"}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	if err := BackfillSchedulerID(ctx, db, "box-a"); err != nil {
		t.Fatalf("BackfillSchedulerID: %v", err)
	}
	for _, tc := range []struct {
		table, query string
	}{{"projects", `SELECT COALESCE(scheduler_id,'') FROM projects WHERE name='legacy-lane'`},
		{"ticks", `SELECT COALESCE(scheduler_id,'') FROM ticks WHERE id='` + tk.ID + `'`},
		{"events", `SELECT COALESCE(scheduler_id,'') FROM events WHERE component='test'`}} {
		var got string
		if err := db.QueryRowContext(ctx, tc.query).Scan(&got); err != nil {
			t.Fatalf("%s backfill readback: %v", tc.table, err)
		}
		if got != "box-a" {
			t.Errorf("%s.scheduler_id = %q after backfill, want box-a (pre-existing rows must carry the local id)", tc.table, got)
		}
	}
}

// TestSchedulerIDStampOnNewRows proves the daemon stamp: with an identity
// installed, every row the write paths create carries it — projects, ticks,
// events (acceptance #2's "stamp scheduler_id on new rows" half).
func TestSchedulerIDStampOnNewRows(t *testing.T) {
	db := newTestDB(t)
	SetSchedulerID("box-test")
	defer func() { SetSchedulerID("") }() // process-global: reset for siblings
	ctx := context.Background()

	p := &Project{Weight: 10, Priority: 5, Name: "stamped-lane", RepoURL: "https://example.com/stamped", Workdir: "/tmp/stamped-lane"}
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	tk := &Tick{ID: NextTickID(ctx, "stamped-lane"), ProjectName: "stamped-lane"}
	if err := CreateTick(ctx, db, tk); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	if err := LogEvent(ctx, db, &Event{Severity: SeverityInfo, Component: "test", Message: "stamped row"}); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	for _, tc := range []struct{ table, query string }{
		{"projects", `SELECT scheduler_id FROM projects WHERE name='stamped-lane'`},
		{"ticks", `SELECT scheduler_id FROM ticks WHERE id='` + tk.ID + `'`},
		{"events", `SELECT scheduler_id FROM events WHERE component='test'`},
	} {
		var got string
		if err := db.QueryRowContext(ctx, tc.query).Scan(&got); err != nil {
			t.Fatalf("%s stamp readback: %v", tc.table, err)
		}
		if got != "box-test" {
			t.Errorf("%s.scheduler_id = %q, want box-test (every row the daemon writes carries its scheduler_id)", tc.table, got)
		}
	}
}

// TestSetSchedulerIDFirstWriterWins pins the stable-across-restarts
// contract at the process level: the first installation wins; later calls
// (a mid-process flap) are ignored.
func TestSetSchedulerIDFirstWriterWins(t *testing.T) {
	if SchedulerID() != "" {
		t.Skip("another test in this binary already installed an identity — the once-guard is proven by TestSchedulerIDStampOnNewRows")
	}
	SetSchedulerID("first-id")
	if SchedulerID() != "first-id" {
		t.Fatalf("SchedulerID = %q, want first-id", SchedulerID())
	}
	SetSchedulerID("second-id")
	if SchedulerID() != "first-id" {
		t.Errorf("SchedulerID = %s after a second SetSchedulerID, want first-id (identity is installed once and never flaps)", SchedulerID())
	}
}

// TestResolveDefaultSchedulerID proves the host-derived default: the short
// hostname (no domain part), never empty.
func TestResolveDefaultSchedulerID(t *testing.T) {
	got := ResolveDefaultSchedulerID()
	if got == "" {
		t.Fatal("ResolveDefaultSchedulerID = \"\", want the short hostname fallback")
	}
	for i := 0; i < len(got); i++ {
		if got[i] == '.' {
			t.Errorf("ResolveDefaultSchedulerID = %q, want the SHORT hostname (no domain part)", got)
			break
		}
	}
}

// TestIsPeerStale_Window proves the freshness predicate: fresh within the
// window, stale past it, and the never-heartbeated/unparseable cases are
// STALE (honest no-signal, never a crash) — the rendering law's engine.
func TestIsPeerStale_Window(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fixed := func() time.Time { return now }
	const window = 180 // seconds

	cases := []struct {
		name        string
		lastContact string
		want        bool
	}{
		{"empty is stale", "", true},
		{"garbage is stale", "not-a-timestamp", true},
		{"just now is fresh", now.Add(-30 * time.Second).UTC().Format(time.RFC3339), false},
		{"at the boundary minus epsilon is fresh", now.Add(-179 * time.Second).UTC().Format(time.RFC3339), false},
		{"past the window is stale", now.Add(-181 * time.Second).UTC().Format(time.RFC3339), true},
		{"far past is stale", now.Add(-24 * time.Hour).UTC().Format(time.RFC3339), true},
	}
	for _, tc := range cases {
		if got := IsPeerStale(tc.lastContact, window, fixed); got != tc.want {
			t.Errorf("%s: IsPeerStale(%q, %d) = %v, want %v", tc.name, tc.lastContact, window, got, tc.want)
		}
	}
}

// TestPeerRegistryUpsertListHeartbeat is the §2 store battery behind the
// three routes: upsert inserts AND refreshes (without touching liveness),
// list returns the documented fields, heartbeat stamps last_contact and
// 404s (ErrPeerNotFound) on an unknown id.
func TestPeerRegistryUpsertListHeartbeat(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Register.
	if err := UpsertPeer(ctx, db, &Peer{ID: "box-b", URL: "http://box-b:9090", Version: "v1", Capabilities: "control"}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
	got, err := GetPeer(ctx, db, "box-b")
	if err != nil {
		t.Fatalf("GetPeer: %v", err)
	}
	if got.URL != "http://box-b:9090" || got.Version != "v1" || got.Capabilities != "control" {
		t.Errorf("registered peer = %+v", got)
	}
	if got.LastContact != "" {
		t.Errorf("fresh registration last_contact = %q, want \"\" (registration is not liveness)", got.LastContact)
	}

	// Registration without a heartbeat renders STALE (never "down").
	if !IsPeerStale(got.LastContact, PeerFreshnessWindowDefault, time.Now) {
		t.Error("never-heartbeated peer rendered fresh — the rendering law requires stale=true")
	}

	// Upsert refreshes identity, keeps liveness: heartbeat first, then a
	// re-registration with a new URL.
	if err := PeerHeartbeat(ctx, db, "box-b"); err != nil {
		t.Fatalf("PeerHeartbeat: %v", err)
	}
	afterHB, err := GetPeer(ctx, db, "box-b")
	if err != nil {
		t.Fatalf("GetPeer after heartbeat: %v", err)
	}
	if afterHB.LastContact == "" {
		t.Fatal("heartbeat did not stamp last_contact")
	}
	if _, perr := time.Parse(time.RFC3339, afterHB.LastContact); perr != nil {
		t.Errorf("last_contact = %s, want RFC3339", afterHB.LastContact)
	}
	if err := UpsertPeer(ctx, db, &Peer{ID: "box-b", URL: "http://box-b:9091", Version: "v2", Capabilities: "control,query"}); err != nil {
		t.Fatalf("UpsertPeer refresh: %v", err)
	}
	refreshed, err := GetPeer(ctx, db, "box-b")
	if err != nil {
		t.Fatalf("GetPeer refreshed: %v", err)
	}
	if refreshed.URL != "http://box-b:9091" || refreshed.Version != "v2" {
		t.Errorf("refreshed peer = %+v, want the new url/version", refreshed)
	}
	if refreshed.LastContact != afterHB.LastContact {
		t.Errorf("re-registration changed last_contact %q -> %q (only a heartbeat refreshes liveness)", afterHB.LastContact, refreshed.LastContact)
	}

	// List carries everything.
	peers, err := ListPeers(ctx, db)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].ID != "box-b" {
		t.Fatalf("ListPeers = %+v, want exactly box-b", peers)
	}

	// Unknown-id heartbeat refuses (the API maps this to 404).
	if err := PeerHeartbeat(ctx, db, "ghost"); err == nil {
		t.Fatal("heartbeat on unknown peer succeeded — want ErrPeerNotFound (no auto-register)")
	}
	// Empty id refuses at both write paths.
	if err := UpsertPeer(ctx, db, &Peer{ID: "  "}); err == nil {
		t.Error("UpsertPeer accepted a blank id")
	}
}

// (contains lives in schedgap215_test.go — reuse, never redeclare.)
