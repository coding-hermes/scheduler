package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SCHED-GAP-16947 acceptance at the STORAGE layer.
//
// The reason code is only real if the database stores AND constrains it.
// Migration v91 adds ticks.reason_code (closed vocabulary + the
// unknown:<raw> escape hatch, enforced by a CHECK), ticks.tokens_out_unavailable
// (the explicit unavailable-with-reason mark) and ticks.reason_code_derivation
// (the repair pass's annotation). The repair pass's own test lives here too,
// because its verdicts are read back through the same columns.

// schedGap16947V90DB builds a genuine pre-16947 (v90) database: the full ladder
// except v91. The premise ("the column does not exist yet") is asserted by the
// caller before any claim about what v91 changed.
func schedGap16947V90DB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "v90.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", dbPath, err)
	}
	t.Cleanup(func() { db.Close() })
	applyTestDurabilityOff(t, db)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS migrations (
    version   INTEGER PRIMARY KEY,
    desc      TEXT NOT NULL,
    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);`); err != nil {
		t.Fatalf("create migrations table: %v", err)
	}

	for _, m := range migrations {
		if m.version >= 91 {
			break // stop before v91 — the migration under test
		}
		if _, err := db.ExecContext(ctx, m.stmt); err != nil {
			// Mirror Migrate's tolerance (see schedgap203b): revised CREATE
			// TABLEs replay as "duplicate column name".
			if !strings.Contains(err.Error(), "duplicate column name") {
				t.Fatalf("apply v%d (%s): %v", m.version, m.desc, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO migrations (version, desc) VALUES (?, ?)`, m.version, m.desc); err != nil {
			t.Fatalf("record v%d: %v", m.version, err)
		}
	}
	return db
}

// tickColumnSet lists ticks' column names via PRAGMA table_info.
func tickColumnSet(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(ticks)`)
	if err != nil {
		t.Fatalf("pragma table_info(ticks): %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info row: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return cols
}

// seedReasonProject inserts the FK parent one tick row needs.
func seedReasonProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, enabled, created_at, updated_at)
		VALUES (?, 'https://example.com/x', '/tmp/x', 1, datetime('now'), datetime('now'))`, name); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
}

// TestSCHEDGAP16947_PreMigrationSchemaLacksReasonColumns asserts the premise:
// the v90 ticks table must LACK all three columns, or the upgrade test proves
// nothing.
func TestSCHEDGAP16947_PreMigrationSchemaLacksReasonColumns(t *testing.T) {
	db := schedGap16947V90DB(t)
	cols := tickColumnSet(t, db)
	for _, c := range []string{"reason_code", "tokens_out_unavailable", "reason_code_derivation"} {
		if cols[c] {
			t.Fatalf("pre-v91 ticks table HAS %s — the premise is broken (nothing for v91 to add)", c)
		}
	}
}

// TestSCHEDGAP16947_UpgradeAddsConstrainedReasonColumns migrates a v90 database
// to HEAD and proves (a) the columns exist with honest defaults, (b) every
// closed-vocabulary value stores, (c) the CHECK rejects a value OUTSIDE the
// vocabulary and a BARE unknown (the falsifier at the schema layer, not just in
// Go), and (d) the unknown:<raw> escape hatch stores.
func TestSCHEDGAP16947_UpgradeAddsConstrainedReasonColumns(t *testing.T) {
	db := schedGap16947V90DB(t)
	ctx := context.Background()

	seedReasonProject(t, db, "vacuum-lane")
	if _, err := db.ExecContext(ctx,
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		 VALUES ('pre-v91-tick', 'vacuum-lane', 'completed', '2026-10-10T00:00:00Z', '2026-10-10T00:00:00Z')`); err != nil {
		t.Fatalf("seed pre-v91 tick: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate to HEAD: %v", err)
	}
	version, err := MigrationVersion(ctx, db)
	if err != nil {
		t.Fatalf("MigrationVersion: %v", err)
	}
	if version != latestMigration {
		t.Fatalf("migration version = %d, want %d", version, latestMigration)
	}
	cols := tickColumnSet(t, db)
	for _, c := range []string{"reason_code", "tokens_out_unavailable", "reason_code_derivation"} {
		if !cols[c] {
			t.Fatalf("post-v91 ticks table LACKS %s — the migration did not add it", c)
		}
	}

	// (a) the legacy row keeps honest defaults: nothing owed, nothing claimed.
	var code, unavailable, derivation string
	if err := db.QueryRowContext(ctx,
		`SELECT reason_code, tokens_out_unavailable, reason_code_derivation FROM ticks WHERE id = 'pre-v91-tick'`,
	).Scan(&code, &unavailable, &derivation); err != nil {
		t.Fatalf("read v91 defaults: %v", err)
	}
	if code != "" || unavailable != "" || derivation != "" {
		t.Errorf("v91 defaults = (%q,%q,%q), want empty triple", code, unavailable, derivation)
	}

	// (b) every closed-vocabulary member stores.
	for _, c := range TickReasonCodes {
		if _, err := db.ExecContext(ctx, `UPDATE ticks SET reason_code = ? WHERE id = 'pre-v91-tick'`, c); err != nil {
			t.Errorf("closed-vocabulary code %q rejected by the CHECK: %v", c, err)
		}
	}
	// (d) the escape hatch stores, raw signal included.
	hatch := TickReasonCodeUnknown("gateway answered 200 with no usage object")
	if _, err := db.ExecContext(ctx, `UPDATE ticks SET reason_code = ? WHERE id = 'pre-v91-tick'`, hatch); err != nil {
		t.Errorf("unknown:<raw> code rejected by the CHECK: %v", err)
	}
	if !strings.HasPrefix(hatch, TickReasonCodeUnknownPrefix) || len(hatch) <= len(TickReasonCodeUnknownPrefix) {
		t.Errorf("TickReasonCodeUnknown(%q) = %q, want the unknown:<raw> form", "gateway answered 200 with no usage object", hatch)
	}

	// (c) the falsifiers: a bogus code and the BARE unknown forms are rejected.
	for _, bad := range []string{"bogus", "dry_run", "UNKNOWN", "unknown", "unknown:"} {
		if _, err := db.ExecContext(ctx, `UPDATE ticks SET reason_code = ? WHERE id = 'pre-v91-tick'`, bad); err == nil {
			t.Errorf("ticks.reason_code accepted %q — the closed-vocabulary CHECK is missing or too loose", bad)
		}
	}
}

// TestSCHEDGAP16947_FreshSchemaHasReasonColumns is the "freshly migrated DB"
// clause: a brand-new install carries the columns and the same constraint.
func TestSCHEDGAP16947_FreshSchemaHasReasonColumns(t *testing.T) {
	db := newTestDB(t)
	cols := tickColumnSet(t, db)
	for _, c := range []string{"reason_code", "tokens_out_unavailable", "reason_code_derivation"} {
		if !cols[c] {
			t.Fatalf("fresh database LACKS ticks.%s — a new install would not carry the reason dimension", c)
		}
	}
	seedReasonProject(t, db, "fresh-lane")
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, spawned_at, created_at)
		VALUES ('fresh-t1', 'fresh-lane', 'completed', '2026-10-10T00:00:00Z', '2026-10-10T00:00:00Z')`); err != nil {
		t.Fatalf("insert fresh tick: %v", err)
	}
	if _, err := db.Exec(`UPDATE ticks SET reason_code = 'not_in_the_vocabulary' WHERE id = 'fresh-t1'`); err == nil {
		t.Fatal("fresh ticks.reason_code accepted a value outside the closed vocabulary")
	}
}

// TestSCHEDGAP16947_VocabularyClosure pins the Go twin of the CHECK: exactly the
// eight documented names (seven stored members + the unknown:<raw> form), no
// more, no fewer.
func TestSCHEDGAP16947_VocabularyClosure(t *testing.T) {
	want := []string{
		"no_work", "model_noop", "no_artifact", "malformed_tool_call",
		"deadline_exceeded", "not_spawned", "lane_unavailable",
	}
	if len(TickReasonCodes) != len(want) {
		t.Fatalf("TickReasonCodes has %d members, want %d: %v", len(TickReasonCodes), len(want), TickReasonCodes)
	}
	for _, w := range want {
		if !TickReasonCodeIsValid(w) {
			t.Errorf("%q is not accepted by TickReasonCodeIsValid", w)
		}
	}
	for _, bad := range []string{"", "bogus", "unknown", "unknown:", "unknown:   ", "unknown:\t"} {
		if TickReasonCodeIsValid(bad) {
			t.Errorf("TickReasonCodeIsValid(%q) = true, want false", bad)
		}
	}
	if !TickReasonCodeIsValid(TickReasonCodeUnknown("raw signal")) {
		t.Error("a well-formed unknown:<raw> code was rejected")
	}
	// Coerce is the write-site guard: a bogus value degrades to an unknown that
	// CARRIES it, and the honest-empty value passes through untouched.
	if got := CoerceTickReasonCode("dry_run"); !strings.HasPrefix(got, TickReasonCodeUnknownPrefix) || !strings.Contains(got, "dry_run") {
		t.Errorf("CoerceTickReasonCode(\"dry_run\") = %q, want unknown: carrying the offender", got)
	}
	if got := CoerceTickReasonCode(""); got != "" {
		t.Errorf("CoerceTickReasonCode(\"\") = %q, want \"\" (productive/legacy convention)", got)
	}
	// The raw signal is whitespace-collapsed and bounded — never empty.
	if got := TickReasonCodeUnknown("  a   b\n c  "); got != "unknown:a b c" {
		t.Errorf("TickReasonCodeUnknown collapsed to %q, want unknown:a b c", got)
	}
	if got := TickReasonCodeUnknown(""); got != "unknown:unspecified" {
		t.Errorf("TickReasonCodeUnknown(\"\") = %q, want unknown:unspecified", got)
	}
	long := TickReasonCodeUnknown(strings.Repeat("x", 500))
	if len([]rune(long)) > len(TickReasonCodeUnknownPrefix)+tickReasonCodeUnknownMaxRaw {
		t.Errorf("TickReasonCodeUnknown did not bound the raw signal: %d runes", len([]rune(long)))
	}
}

// schedGap16947Tick is the seed shape the repair assertions read back.
type schedGap16947Tick struct {
	id, project, outcome, spawnedAt string
	tokensIn, tokensOut             int
	commits, files, memory          int
}

// seedRepairTick writes one concluded tick with an explicit evidence shape.
func seedRepairTick(t *testing.T, db *sql.DB, tk schedGap16947Tick) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, outcome, spawned_at, completed_at,
		 tokens_in, tokens_out, commits, files_changed, memory_keys, created_at)
		VALUES (?, ?, 'completed', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tk.id, tk.project, tk.outcome, tk.spawnedAt, tk.spawnedAt,
		tk.tokensIn, tk.tokensOut, tk.commits, tk.files, tk.memory, tk.spawnedAt); err != nil {
		t.Fatalf("seed repair tick %s: %v", tk.id, err)
	}
}

// TestSCHEDGAP16947_RepairReclassifiesBlackout is the acceptance for deliverable
// (e): the 2026-10-10..12 block is reclassified from evidence still present, the
// derivation is ANNOTATED on the row, history is otherwise untouched, and a
// second pass writes nothing.
func TestSCHEDGAP16947_RepairReclassifiesBlackout(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, p := range []string{"dark-lane", "busy-lane"} {
		seedReasonProject(t, db, p)
	}

	// busy-lane: a productive tick in the window (fleet aliveness evidence) plus
	// one zero-token dry tick of its own.
	seedRepairTick(t, db, schedGap16947Tick{
		id: "busy-productive", project: "busy-lane", outcome: "committed",
		spawnedAt: "2026-10-10T04:00:00Z", commits: 3, files: 7,
	})
	seedRepairTick(t, db, schedGap16947Tick{
		id: "busy-dry-silent", project: "busy-lane", outcome: "dry_run",
		spawnedAt: "2026-10-10T05:00:00Z",
	})
	// dark-lane: nothing productive all window — the blackout shape.
	seedRepairTick(t, db, schedGap16947Tick{
		id: "dark-dry-1", project: "dark-lane", outcome: "dry_run", spawnedAt: "2026-10-10T06:00:00Z",
	})
	seedRepairTick(t, db, schedGap16947Tick{
		id: "dark-dry-2", project: "dark-lane", outcome: "dry_run", spawnedAt: "2026-10-11T06:00:00Z",
	})
	// A dry tick that DID spend tokens: the session ran, nothing landed.
	seedRepairTick(t, db, schedGap16947Tick{
		id: "dark-spent", project: "dark-lane", outcome: "dry_run", spawnedAt: "2026-10-11T07:00:00Z",
		tokensIn: 900_000, tokensOut: 4_000,
	})
	// An unmeasured artifact delta (-1): cannot be repaired into "no artifact".
	seedRepairTick(t, db, schedGap16947Tick{
		id: "dark-unmeasured", project: "dark-lane", outcome: "dry_run", spawnedAt: "2026-10-12T07:00:00Z",
		commits: -1, files: -1,
	})
	// Outside the window: must stay untouched.
	seedRepairTick(t, db, schedGap16947Tick{
		id: "outside-dry", project: "dark-lane", outcome: "dry_run", spawnedAt: "2026-10-20T07:00:00Z",
	})

	from := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

	// DRY RUN first: identical report, nothing written.
	before, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{From: from, To: to})
	if err != nil {
		t.Fatalf("dry-run repair: %v", err)
	}
	if before.Apply {
		t.Error("dry-run report claims Apply=true")
	}
	if before.Applied != 0 {
		t.Errorf("dry-run applied %d rows, want 0", before.Applied)
	}
	if before.Candidates != 4 {
		t.Errorf("dry-run candidates = %d, want 4 (dark-dry-1, dark-dry-2, dark-spent, busy-dry-silent)", before.Candidates)
	}
	if before.Unmeasured != 1 {
		t.Errorf("dry-run unmeasured = %d, want 1 (the -1 sentinel row)", before.Unmeasured)
	}
	var code string
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='dark-dry-1'`).Scan(&code); err != nil {
		t.Fatalf("read code after dry run: %v", err)
	}
	if code != "" {
		t.Fatalf("dry run wrote reason_code=%q — a dry run must not touch the database", code)
	}

	// APPLY.
	rep, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{From: from, To: to, Apply: true})
	if err != nil {
		t.Fatalf("apply repair: %v", err)
	}
	if rep.Applied != 4 || rep.Candidates != 4 {
		t.Fatalf("applied %d of %d candidates, want 4 of 4 (classes=%v)", rep.Applied, rep.Candidates, rep.Classes)
	}

	wantCodes := map[string]string{
		"dark-dry-1":      TickReasonCodeLaneUnavailable,   // lane dark all window, fleet alive
		"dark-dry-2":      TickReasonCodeLaneUnavailable,   // same class, derived per row
		"dark-spent":      TickReasonCodeNoArtifact,        // tokens spent, nothing landed
		"busy-dry-silent": TickReasonCodeMalformedToolCall, // lane alive, session silent
	}
	for id, want := range wantCodes {
		var got, derivation string
		if err := db.QueryRow(`SELECT reason_code, reason_code_derivation FROM ticks WHERE id = ?`, id).
			Scan(&got, &derivation); err != nil {
			t.Fatalf("read repaired row %s: %v", id, err)
		}
		if got != want {
			t.Errorf("%s reason_code = %q, want %q", id, got, want)
		}
		if !strings.HasPrefix(derivation, "repair:class=") {
			t.Errorf("%s derivation = %q, want a repair:class=… annotation", id, derivation)
		}
		if !strings.Contains(derivation, "lane_productive_ticks=") || !strings.Contains(derivation, "fleet_productive_ticks=") {
			t.Errorf("%s derivation omits the productivity evidence: %q", id, derivation)
		}
	}
	var darkDerivation string
	if err := db.QueryRow(`SELECT reason_code_derivation FROM ticks WHERE id='dark-dry-1'`).Scan(&darkDerivation); err != nil {
		t.Fatalf("read dark derivation: %v", err)
	}
	if !strings.Contains(darkDerivation, "three_day_lane_blackout") {
		t.Errorf("dark-lane derivation %q does not name the blackout class", darkDerivation)
	}

	// The -1 row keeps NO code: an unobserved artifact cannot be repaired into
	// "none" (the SCHED-GAP-1652 sentinel doctrine).
	var unmeasuredCode string
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='dark-unmeasured'`).Scan(&unmeasuredCode); err != nil {
		t.Fatalf("read unmeasured row: %v", err)
	}
	if unmeasuredCode != "" {
		t.Errorf("unmeasured row got reason_code=%q, want \"\" (left alone)", unmeasuredCode)
	}

	// History is annotated, not rewritten: status/outcome/error/tokens survive.
	var status, outcome string
	var tokensIn, commits int
	if err := db.QueryRow(`SELECT status, outcome, tokens_in, commits FROM ticks WHERE id='dark-spent'`).
		Scan(&status, &outcome, &tokensIn, &commits); err != nil {
		t.Fatalf("read dark-spent history: %v", err)
	}
	if status != "completed" || outcome != "dry_run" || tokensIn != 900_000 || commits != 0 {
		t.Errorf("repair rewrote history on dark-spent: status=%q outcome=%q tokens_in=%d commits=%d", status, outcome, tokensIn, commits)
	}

	// Out-of-window rows are untouched.
	var outside string
	if err := db.QueryRow(`SELECT reason_code FROM ticks WHERE id='outside-dry'`).Scan(&outside); err != nil {
		t.Fatalf("read out-of-window row: %v", err)
	}
	if outside != "" {
		t.Errorf("out-of-window tick was repaired (reason_code=%q)", outside)
	}

	// IDEMPOTENT: a second apply writes nothing and reports the first pass's
	// work as already stamped.
	again, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{From: from, To: to, Apply: true})
	if err != nil {
		t.Fatalf("second repair pass: %v", err)
	}
	if again.Applied != 0 || again.Candidates != 0 {
		t.Errorf("second pass applied=%d candidates=%d, want 0/0 (idempotent)", again.Applied, again.Candidates)
	}
	if again.AlreadyStamped != 4 {
		t.Errorf("second pass already_stamped = %d, want 4", again.AlreadyStamped)
	}

	// The pass left ONE audit event naming the window.
	var events int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE component='repair'`).Scan(&events); err != nil {
		t.Fatalf("count repair events: %v", err)
	}
	if events != 1 {
		t.Errorf("repair audit events = %d, want exactly 1 summary event", events)
	}
}

// TestSCHEDGAP16947_RepairRefusesFleetWideSilence proves the honesty floor: when
// NOTHING in the window produced an artifact, a zero-token dry tick cannot be
// attributed to the lane, so the code keeps the raw signal instead of claiming
// a blackout.
func TestSCHEDGAP16947_RepairRefusesFleetWideSilence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seedReasonProject(t, db, "dead-fleet-lane")
	seedRepairTick(t, db, schedGap16947Tick{
		id: "dead-dry", project: "dead-fleet-lane", outcome: "dry_run", spawnedAt: "2026-10-11T01:00:00Z",
	})
	from := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	rep, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{From: from, To: to, Apply: true})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if rep.Applied != 1 {
		t.Fatalf("applied = %d, want 1", rep.Applied)
	}
	var code, derivation string
	if err := db.QueryRow(`SELECT reason_code, reason_code_derivation FROM ticks WHERE id='dead-dry'`).
		Scan(&code, &derivation); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if !strings.HasPrefix(code, TickReasonCodeUnknownPrefix) {
		t.Errorf("fleet-wide-silence row reason_code = %q, want an unknown:<raw> code", code)
	}
	if !strings.Contains(code, "fleet silent in window") {
		t.Errorf("unknown code %q does not carry the raw signal", code)
	}
	if !strings.Contains(derivation, "fleet_wide_silence_unattributable") {
		t.Errorf("derivation %q does not name the unattributable class", derivation)
	}
	// The raw signal must still be a legal code.
	if !TickReasonCodeIsValid(code) {
		t.Errorf("repaired unknown code %q is not a valid vocabulary member", code)
	}
}

// TestSCHEDGAP16947_UsagePoolExpiryStampsReasonCode closes the last conclusion
// path that bypasses lifecycle.Complete: a lease whose owner exited mid-tick is
// terminalized by the usage-pool recovery pass, which must stamp the same
// owner-gone class.
func TestSCHEDGAP16947_UsagePoolExpiryStampsReasonCode(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := ConfigureUsagePools(ctx, db, []UsagePool{{ID: "p16947", Kind: "project", ActiveLimit: 2, Enabled: true}}, nil); err != nil {
		t.Fatalf("configure usage pools: %v", err)
	}
	seedReasonProject(t, db, "pool-lane")
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at) VALUES ('pool-tick', 'pool-lane', 'queued', 'now')`); err != nil {
		t.Fatalf("seed tick: %v", err)
	}
	if _, err := ReserveAll(ctx, db, "pool-lease", "pool-tick", "pool-lane", []string{"p16947"}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := db.Exec(`UPDATE usage_pool_leases SET owner_id='pid:2147483647', expires_at='2000-01-01T00:00:00Z' WHERE lease_id='pool-lease'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if n, err := RecoverUsagePoolLeases(ctx, db); err != nil || n != 1 {
		t.Fatalf("recovered=%d err=%v, want 1", n, err)
	}
	var status, code string
	if err := db.QueryRow(`SELECT status, reason_code FROM ticks WHERE id='pool-tick'`).Scan(&status, &code); err != nil {
		t.Fatalf("read expired-lease tick: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if code != TickReasonCodeLaneUnavailable {
		t.Errorf("reason_code = %q, want %q (the owner-gone class)", code, TickReasonCodeLaneUnavailable)
	}
}

// TestSCHEDGAP16947_RepairRequiresWindow pins the guard rail: an unbounded pass
// over tick history is refused.
func TestSCHEDGAP16947_RepairRequiresWindow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{Apply: true}); err == nil {
		t.Fatal("repair with no window bounds succeeded, want an error")
	}
	from := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if _, err := RepairTickReasonCodes(ctx, db, ReasonCodeRepairOpts{From: from, To: to}); err == nil {
		t.Fatal("repair with an inverted window succeeded, want an error")
	}
}
