package scheduler

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"

	_ "modernc.org/sqlite"
)

// ── S12 §4.3 (SCHED-GAP-111): effective tick deadline ────────────────────
//
// The resolver under test: effectiveTickTimeout resolves, in ONE place, the
// deadline Spawn() applies to a foreman session. Order:
// SCHEDULER_WAVE_TICK_TIMEOUT env > namespaces.wave_tick_timeout > s.timeout.
// Every failure path inherits s.timeout; waves-off namespaces are
// byte-identical to pre-S12 behavior.

// newWaveTestSpawner opens an in-memory DB with the REAL namespaces table
// (migration ladder via database.InitDB) so the resolver's indexed lookup
// runs against the production schema.
func newWaveTestSpawner(t *testing.T, timeout time.Duration) *Spawner {
	t.Helper()
	db := newTestDB(t) // database.InitDB(":memory:") — full schema incl. v27 wave columns
	s := NewSpawner(db, 4, timeout)
	return s
}

// insertWaveNamespace creates a namespace row with the given wave columns.
func insertWaveNamespace(t *testing.T, s *Spawner, id string, enabled bool, tickTimeout string) {
	t.Helper()
	var en int
	if enabled {
		en = 1
	}
	if _, err := s.db.Exec(
		`INSERT INTO namespaces (id, weight, reserved, hard_cap, max_concurrent, enabled, wave_enabled, wave_tick_timeout, wave_workers_cap)
		 VALUES (?, 10, 1, 100, 0, 1, ?, ?, 0)`,
		id, en, tickTimeout); err != nil {
		t.Fatalf("insert namespace %s: %v", id, err)
	}
}

// TestEffectiveTickTimeout_WaveNamespace (§12): a project whose namespace
// has wave_enabled=1 and wave_tick_timeout="3h" resolves the session deadline
// to 3h — not the base --tick-timeout.
func TestEffectiveTickTimeout_WaveNamespace(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "h3", true, "3h")

	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "h3"})
	if got != 3*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 3h (wave-enabled namespace override)", got)
	}
}

// TestEffectiveTickTimeout_SerialNamespaceInherits (§12): every shape with NO
// deadline configuration inherits s.timeout — no namespace id, unknown
// namespace, wave_enabled=1 with an empty wave_tick_timeout, and an
// unparseable timeout (WARN+inherit). SCHED-GAP-1691: a wave_enabled=0
// namespace with a timeout set is NO LONGER in this list — it now resolves
// the override (TestEffectiveTickTimeout_WaveOffNamespaceWithTimeout).
func TestEffectiveTickTimeout_SerialNamespaceInherits(t *testing.T) {
	base := 90 * time.Minute
	s := newWaveTestSpawner(t, base)

	insertWaveNamespace(t, s, "empty-timeout", true, "") // waves on, no timeout → inherit
	insertWaveNamespace(t, s, "garbage-timeout", true, "later")

	cases := []struct {
		name string
		ns   string
	}{
		{"no namespace id", ""},
		{"wave enabled, empty timeout", "empty-timeout"},
		{"wave enabled, unparseable timeout (WARN+inherit)", "garbage-timeout"},
		{"unknown namespace", "does-not-exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: tc.ns})
			if got != base {
				t.Errorf("effectiveTickTimeout(ns=%q) = %v, want base %v", tc.ns, got, base)
			}
		})
	}
}

// TestEffectiveTickTimeout_EnvOverrideWins: SCHEDULER_WAVE_TICK_TIMEOUT
// beats the namespace value.
func TestEffectiveTickTimeout_EnvOverrideWins(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "h3", true, "3h")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "90m")
	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "h3"})
	if got != 90*time.Minute {
		t.Errorf("effectiveTickTimeout = %v, want 90m (env override beats namespace 3h)", got)
	}
}

// TestEffectiveTickTimeout_UnparseableEnvFallsBack: a garbage env value WARNs
// and falls back to the NAMESPACE value (not to base) — the namespace is
// still wave-enabled and carries a valid override.
func TestEffectiveTickTimeout_UnparseableEnvFallsBack(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "h3", true, "3h")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "not-a-duration")
	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "h3"})
	if got != 3*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 3h (unparseable env falls back to namespace value)", got)
	}
}

// TestEffectiveTickTimeout_EnvIgnoredWithoutDeadlineConfig (SCHED-GAP-1691):
// the env override never touches a namespace with NO deadline configuration
// (wave_enabled=0 AND empty wave_tick_timeout) — a global env bump must not
// hand every plain project an extended deadline. A wave-off namespace that
// DOES carry a wave_tick_timeout participates in the env cascade normally
// (TestEffectiveTickTimeout_EnvAppliesToWaveOffWithTimeout).
func TestEffectiveTickTimeout_EnvIgnoredWithoutDeadlineConfig(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "serial", false, "")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "4h")
	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "serial"})
	if got != base {
		t.Errorf("effectiveTickTimeout = %v, want base %v — env override leaked into a namespace with no deadline config", got, base)
	}
}

// TestEffectiveTickTimeout_CeilingClamp: a resolved WAVE override above 4h
// (here via env — config validation rejects the file form) is clamped to 4h
// at the resolver, the second line of defense behind config validation.
func TestEffectiveTickTimeout_CeilingClamp(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "h3", true, "3h")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "6h")
	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "h3"})
	if got != 4*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 4h (ceiling clamp)", got)
	}
}

// TestEffectiveTickTimeout_BaseNeverClamped: a base --tick-timeout above 4h
// (out of scope per the brief) passes through the resolver untouched.
func TestEffectiveTickTimeout_BaseNeverClamped(t *testing.T) {
	s := newWaveTestSpawner(t, 6*time.Hour)
	insertWaveNamespace(t, s, "serial", false, "")

	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "serial"})
	if got != 6*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 6h base passed through (base is never clamped)", got)
	}
}

// TestEffectiveTickTimeout_DBLookupErrorInherits: a resolver DB error must
// fall back to base — never kill the spawn.
func TestEffectiveTickTimeout_DBLookupErrorInherits(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// No tables: every query errors. A bare spawner over it must still
	// resolve the base timeout for a namespaced project.
	s := NewSpawner(db, 4, 2*time.Hour)
	got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "any"})
	if got != 2*time.Hour {
		t.Errorf("effectiveTickTimeout on broken DB = %v, want base 2h", got)
	}
}

// TestEffectiveTickTimeout_WaveOffNamespaceWithTimeout (SCHED-GAP-1691): a
// namespace with wave_enabled=0 that carries wave_tick_timeout="3h" resolves
// 3h — the per-namespace tick wall is decoupled from waves. The spawner's
// base timeout is untouched: a project in the same fleet WITHOUT the
// namespace override still inherits the base 2h.
func TestEffectiveTickTimeout_WaveOffNamespaceWithTimeout(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "qa", false, "3h")

	got := s.effectiveTickTimeout(PackedProject{Name: "qa-lane", NamespaceID: "qa"})
	if got != 3*time.Hour {
		t.Errorf("effectiveTickTimeout(qa, wave_enabled=0, 3h) = %v, want 3h (per-namespace deadline decoupled from waves)", got)
	}
	// Control: a plain (no-deadline-config) namespace in the same spawner
	// still inherits the base — the 3h is not a fleet-wide bump.
	insertWaveNamespace(t, s, "plain", false, "")
	if got := s.effectiveTickTimeout(PackedProject{Name: "p", NamespaceID: "plain"}); got != base {
		t.Errorf("effectiveTickTimeout(plain) = %v, want base %v — override leaked into a namespace with no deadline config", got, base)
	}
	if s.timeout != base {
		t.Errorf("spawner base timeout mutated to %v, want %v", s.timeout, base)
	}
}

// TestEffectiveTickTimeout_EnvAppliesToWaveOffWithTimeout (SCHED-GAP-1691):
// once a wave_enabled=0 namespace carries an explicit wave_tick_timeout, the
// env override governs its cascade exactly like a wave-enabled namespace —
// and an unparseable env value still falls through to the namespace value.
func TestEffectiveTickTimeout_EnvAppliesToWaveOffWithTimeout(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "qa", false, "3h")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "90m")
	if got := s.effectiveTickTimeout(PackedProject{Name: "qa-lane", NamespaceID: "qa"}); got != 90*time.Minute {
		t.Errorf("effectiveTickTimeout = %v, want 90m (env beats namespace 3h, wave_enabled=0)", got)
	}

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "not-a-duration")
	if got := s.effectiveTickTimeout(PackedProject{Name: "qa-lane", NamespaceID: "qa"}); got != 3*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 3h (unparseable env falls back to the wave-off namespace's value)", got)
	}
}

// TestEffectiveTickTimeout_CeilingClampWaveOff (SCHED-GAP-1691): the 4h
// ceiling clamps a wave_enabled=0 namespace's resolved override too (here
// via env; a hand-edited 5h row takes the same path through clampWaveTimeout).
func TestEffectiveTickTimeout_CeilingClampWaveOff(t *testing.T) {
	base := 2 * time.Hour
	s := newWaveTestSpawner(t, base)
	insertWaveNamespace(t, s, "qa", false, "3h")

	t.Setenv("SCHEDULER_WAVE_TICK_TIMEOUT", "6h")
	if got := s.effectiveTickTimeout(PackedProject{Name: "qa-lane", NamespaceID: "qa"}); got != 4*time.Hour {
		t.Errorf("effectiveTickTimeout = %v, want 4h (ceiling clamp, wave_enabled=0 namespace)", got)
	}
}

// TestFeatureNoWaveTickForWaveOffDeadlineOnly: the FeatureWaveTicks metric
// stays wave-gated (SCHED-GAP-131 semantics preserved). A wave_enabled=0
// namespace with a deadline override must NOT record a wave-tick feature use.
func TestFeatureNoWaveTickForWaveOffDeadlineOnly(t *testing.T) {
	featureUsageIsolated(t)
	db := newTestDB(t)

	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, max_concurrent, wave_enabled, wave_tick_timeout) VALUES ('qa', 10, 0, 0, '3h')`); err != nil {
		t.Fatalf("insert qa namespace: %v", err)
	}
	sp := NewSpawner(db, 4)
	if got := sp.effectiveTickTimeout(PackedProject{NamespaceID: "qa"}); got != 3*time.Hour {
		t.Fatalf("effectiveTickTimeout = %v, want 3h (precondition: the override resolves)", got)
	}

	counts := featureUsageCounts(t, db)
	if counts[database.FeatureWaveTicks] != 0 {
		t.Errorf("wave_ticks count = %d, want 0 (wave_enabled=0 namespace must not count as wave-tick feature use) (%v)",
			counts[database.FeatureWaveTicks], counts)
	}
}

// TestSpawn_GatewayDeadlineUsesWaveTimeout: SPAWN-PATH proof (gateway stub
// harness). The stub server sleeps LONGER than the base --tick-timeout but
// SHORTER than the wave override, then returns a valid response. Under the
// old code (ctx = s.timeout) the request would be canceled at base timeout
// and the tick would fail; with the resolver wired into spawn.go the session
// ctx carries the wave deadline and the tick COMPLETES. No 3h sleep — the
// base is 300ms, the wave override 2s, the stub sleeps 800ms.
func TestSpawn_GatewayDeadlineUsesWaveTimeout(t *testing.T) {
	base := 300 * time.Millisecond
	wave := 2 * time.Second
	stubSleep := 800 * time.Millisecond

	db := newTestDB(t)
	s := NewSpawner(db, 4, base)

	// Namespace with a sub-second-parseable wave timeout.
	if _, err := db.Exec(
		`INSERT INTO namespaces (id, weight, reserved, hard_cap, max_concurrent, enabled, wave_enabled, wave_tick_timeout, wave_workers_cap)
		 VALUES ('wave-ns', 10, 1, 100, 0, 1, 1, ?, 0)`,
		wave.String()); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}

	var handled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(stubSleep) // exceeds base (300ms), under wave (2s)
		handled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_wave_111","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`))
	}))
	defer srv.Close()

	s.SetGatewayClient(NewGatewayClient(srv.URL, "sk-test", 5*time.Second))
	s.SetNoExecFallback(true)

	project := PackedProject{
		Name:        "wave-deadline-proj",
		Workdir:     t.TempDir(),
		NamespaceID: "wave-ns",
	}
	tick, err := s.Spawn(project, "schedgap111-wave-deadline")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — the gateway session ctx did NOT carry the wave deadline "+
			"(stub slept %v > base %v, under wave %v; err=%q)",
			outcome.Status, TickCompleted, stubSleep, base, wave, outcome.Error)
	}
	if !handled.Load() {
		t.Error("gateway stub never handled the request")
	}
}

// TestSpawn_GatewayDeadlineBaseForSerial: the inverse control — same stub
// sleep, NO wave namespace. The session ctx must stay at base timeout, the
// request is canceled, and the tick fails: proves the resolver is actually
// wired (not just lengthening every deadline).
func TestSpawn_GatewayDeadlineBaseForSerial(t *testing.T) {
	base := 300 * time.Millisecond
	stubSleep := 800 * time.Millisecond

	db := newTestDB(t)
	s := NewSpawner(db, 4, base)

	var handled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(stubSleep) // exceeds base 300ms
		handled.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"resp_serial_111","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`))
	}))
	defer srv.Close()

	s.SetGatewayClient(NewGatewayClient(srv.URL, "sk-test", 5*time.Second))
	s.SetNoExecFallback(true)

	// No namespace at all — serial project. With noExecFallback the
	// timed-out gateway call DROPS the tick: Spawn returns an error and no
	// tick. That error IS the proof the session ctx stayed at base timeout.
	project := PackedProject{Name: "serial-deadline-proj", Workdir: t.TempDir()}
	tick, err := s.Spawn(project, "schedgap111-serial-deadline")
	if err == nil {
		// Defensive: if a tick came back, it must NOT be completed.
		if tick != nil {
			outcome := tick.Wait()
			if outcome.Status == TickCompleted {
				t.Errorf("serial tick COMPLETED a request that slept %v past the base %v deadline; "+
					"the base deadline is not being enforced", stubSleep, base)
			}
		} else {
			t.Error("Spawn returned nil tick and nil error")
		}
		t.Logf("Spawn err (dropped, non-completed): %v", err)
		return
	}
	t.Logf("Spawn dropped serial tick at base deadline as expected: %v", err)
}
