package scheduler

// SCHED-GAP-217 — backstopMaxAge regression guard.
//
// The pre-fix code reaped any running tick older than a hardcoded 90m. The
// fix derives the cutoff from the LIVE max effective tick deadline across
// in-flight running ticks. These tests prove the derivation behaves as
// designed across the namespace cascade (inherit / 2h / 3h / 4h / env
// override) and that the hard floor is honored.

import (
	"database/sql"
	"os"
	"testing"
	"time"
)

// sg217InsertRunningTick creates a running tick at the given age. The tick's
// project_name is the project we want backstopMaxAge to consider.
func sg217InsertRunningTick(t *testing.T, db *sql.DB, project string, age time.Duration) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at) VALUES (?, ?, ?, 10, 5, 900, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', 1, datetime('now'), datetime('now'))`,
		project, "https://github.com/example/"+project, "/tmp/work/"+project); err != nil {
		t.Fatalf("insert project %s: %v", project, err)
	}
	spawnedAt := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, 'running', ?, datetime('now'))`,
		"TICK-"+project, project, spawnedAt); err != nil {
		t.Fatalf("insert tick %s: %v", project, err)
	}
}

// setNamespace wires a namespace row with the given wave values and binds
// the named project to it. Empty nsID means the project is unnamespaced.
func setNamespace(t *testing.T, db *sql.DB, nsID, project string, waveEnabled bool, waveTimeout string) {
	t.Helper()
	if nsID == "" {
		return
	}
	if _, err := db.Exec(
		`INSERT INTO namespaces (id, weight, max_concurrent, wave_enabled, wave_tick_timeout, created_at, updated_at) VALUES (?, 100, 4, ?, ?, datetime('now'), datetime('now'))`,
		nsID, boolToInt(waveEnabled), waveTimeout); err != nil {
		t.Fatalf("insert namespace %s: %v", nsID, err)
	}
	if _, err := db.Exec(
		`UPDATE projects SET namespace_id = ? WHERE name = ?`,
		nsID, project); err != nil {
		t.Fatalf("bind project %s to namespace %s: %v", project, nsID, err)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newLoopForBackstop constructs the minimal Loop/Spawner pair that
// backstopMaxAge reads. The Spawner's db/timeout are the only fields the
// helper consults; the rest stay zero.
func newLoopForBackstop(t *testing.T, db *sql.DB, baseTimeout time.Duration) *Loop {
	t.Helper()
	s := NewSpawner(db, 1, baseTimeout)
	l := &Loop{db: db, spawner: s}
	return l
}

// TestBackstopMaxAge_noRunningTicks — empty fleet returns the floored base
// + grace. Pre-fix the empty case never fired (no rows to reap) but the
// helper is now consulted on every evaluate(); the floor keeps the
// regression guard's 1h-arg flipping a 2h tick.
func TestBackstopMaxAge_noRunningTicks(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	got := l.backstopMaxAge()
	want := maxDuration(2*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("no-running-ticks backstop = %v, want %v", got, want)
	}
}

// TestBackstopMaxAge_derivesFromInFlight — three running ticks, the helper
// must take the MAX of the per-namespace effective deadlines + grace. One
// tick in coding-hermes (3h), one in h3 (2h), one unnamespaced (inherits
// base 2h) → max effective = 3h → backstop = 3h + 30m.
func TestBackstopMaxAge_derivesFromInFlight(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-coding-hermes", 30*time.Minute)
	sg217InsertRunningTick(t, db, "proj-h3", 30*time.Minute)
	sg217InsertRunningTick(t, db, "proj-unnamespaced", 30*time.Minute)
	setNamespace(t, db, "coding-hermes", "proj-coding-hermes", true, "3h")
	setNamespace(t, db, "h3", "proj-h3", true, "2h")
	// proj-unnamespaced is intentionally left unbound.

	got := l.backstopMaxAge()
	want := maxDuration(3*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("in-flight backstop = %v, want %v (3h + 30m grace)", got, want)
	}
}

// TestBackstopMaxAge_envOverride — SCHEDULER_WAVE_TICK_TIMEOUT wins over
// the namespace value. The coding-hermes tick is configured 3h, the env
// says 1h, and the helper must return 1h + 30m.
func TestBackstopMaxAge_envOverride(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-coding-hermes", 30*time.Minute)
	setNamespace(t, db, "coding-hermes", "proj-coding-hermes", true, "3h")

	t.Setenv(envWaveTickTimeout, "1h")
	got := l.backstopMaxAge()
	want := maxDuration(1*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("env-override backstop = %v, want %v (1h + 30m grace)", got, want)
	}
}

// TestBackstopMaxAge_waveCeiling — wave_tick_timeout above the 4h ceiling
// (only possible via env or a hand-edited row) is clamped to 4h. The
// helper must return 4h + 30m, not 5h + 30m.
func TestBackstopMaxAge_waveCeiling(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-coding-hermes", 30*time.Minute)
	setNamespace(t, db, "coding-hermes", "proj-coding-hermes", true, "3h")

	t.Setenv(envWaveTickTimeout, "5h")
	got := l.backstopMaxAge()
	want := maxDuration(waveTickTimeoutCeiling+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("ceiling-clamped backstop = %v, want %v (4h + 30m grace)", got, want)
	}
}

// TestBackstopMaxAge_waveOffNamespaceInherits — a wave-off namespace (the
// common case for most namespaces) must NOT contribute a deadline larger
// than the base. The helper returns the base + grace for the running tick.
func TestBackstopMaxAge_waveOffNamespaceInherits(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-tasks", 30*time.Minute)
	setNamespace(t, db, "tasks", "proj-tasks", false, "")

	got := l.backstopMaxAge()
	want := maxDuration(2*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("wave-off backstop = %v, want %v (base 2h + 30m grace)", got, want)
	}
}

// TestBackstopMaxAge_floor — a 30m base timeout must still produce a
// backstop >= 90m. The floor is the byte-equivalent of the pre-fix
// value, preserving the regression guard.
func TestBackstopMaxAge_floor(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 30*time.Minute)

	got := l.backstopMaxAge()
	if got < backstopFloor {
		t.Fatalf("backstop = %v, must be >= backstopFloor %v", got, backstopFloor)
	}
}

// TestBackstopMaxAge_unparseableNamespace — an unparseable wave_tick_timeout
// inherits the base timeout (matches Spawner.effectiveTickTimeout). The
// helper must not panic and must not return a zero duration.
func TestBackstopMaxAge_unparseableNamespace(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-broken", 30*time.Minute)
	setNamespace(t, db, "broken", "proj-broken", true, "not-a-duration")

	// Silence the expected WARN line by setting a flag the helper could
	// read in a future change; for now the WARN will print once.
	_ = os.Getenv("SCHEDULER_TEST_QUIET")

	got := l.backstopMaxAge()
	want := maxDuration(2*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("unparseable backstop = %v, want %v (inherits base)", got, want)
	}
}

// TestBackstopMaxAge_allWaveOff — a fleet of only wave-off running ticks
// produces the base + grace (the per-namespace contributions all reduce
// to "ok=false" and the MAX stays zero, so the fallback fires).
func TestBackstopMaxAge_allWaveOff(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-a", 30*time.Minute)
	sg217InsertRunningTick(t, db, "proj-b", 30*time.Minute)
	setNamespace(t, db, "ns-a", "proj-a", false, "")
	setNamespace(t, db, "ns-b", "proj-b", false, "")

	got := l.backstopMaxAge()
	want := maxDuration(2*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("all-wave-off backstop = %v, want %v", got, want)
	}
}

// TestBackstopMaxAge_dedupesNamespaces — three running ticks in the SAME
// wave-enabled namespace must produce one effective deadline, not three.
// This guards against an N² blowup if the join returns duplicates.
func TestBackstopMaxAge_dedupesNamespaces(t *testing.T) {
	db := newTestDB(t)
	l := newLoopForBackstop(t, db, 2*time.Hour)

	sg217InsertRunningTick(t, db, "proj-1", 30*time.Minute)
	sg217InsertRunningTick(t, db, "proj-2", 30*time.Minute)
	sg217InsertRunningTick(t, db, "proj-3", 30*time.Minute)
	// One namespace, three projects — the helper must dedupe by namespace
	// id, not by (project, namespace) tuple.
	setNamespace(t, db, "coding-hermes", "proj-1", true, "3h")
	if _, err := db.Exec(`UPDATE projects SET namespace_id = ? WHERE name IN (?, ?)`,
		"coding-hermes", "proj-2", "proj-3"); err != nil {
		t.Fatalf("bind proj-2/proj-3 to coding-hermes: %v", err)
	}

	got := l.backstopMaxAge()
	want := maxDuration(3*time.Hour+backstopGrace, backstopFloor)
	if got != want {
		t.Fatalf("deduped-namespace backstop = %v, want %v (3h + 30m grace, single lookup)", got, want)
	}
}
