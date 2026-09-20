package scheduler

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// REG-002 — "parked is not abandoned"
//
// An operator-set cooldown is a DELIBERATE park, not a failure. It must
// survive a store close/reopen (internal/database/reg002_park_persistence_test.go),
// must never be escalated below the value in force when the project was
// enabled, must not escalate at all while the project is disabled, and must
// reset cleanly when the project is re-enabled.
//
// The three tests below pin the scheduler half of that invariant. Every
// assertion here was read out of the source and then verified by live probe
// before being written down; where the board row's wording and the source
// disagree, the SOURCE is what these tests assert, and the disagreement is
// called out in the test comments and in the change report.

// reg002InsertArmedProject inserts a project row with explicit adaptive-cooldown
// policy columns plus an explicit `enabled` flag. It mirrors the existing
// insertAdaptiveProject helper (which hardcodes enabled=1) so the disabled arm
// of these tests can be built without touching the update-layer normalization.
func reg002InsertArmedProject(t *testing.T, db *sql.DB, name string, cd, floor, ceiling, threshold, streak, enabled int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
		 no_progress_threshold, no_progress_ticks, board_rows_seen, board_open_seen)
		VALUES (?, ?, ?, 10, 5, ?, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', ?,
		        datetime('now'), datetime('now'), 1, ?, ?, ?, ?, -1, -1)`,
		name, "https://github.com/example/"+name, filepath.Join(t.TempDir(), name), cd,
		enabled, floor, ceiling, threshold, streak,
	)
	if err != nil {
		t.Fatalf("insert armed project %s: %v", name, err)
	}
}

// reg002AdaptiveState reads the adaptive/cooldown columns under test.
func reg002AdaptiveState(t *testing.T, db *sql.DB, name string) (cd, floor, ceiling, threshold, streak, enabled int) {
	t.Helper()
	err := db.QueryRow(`SELECT cooldown_s, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_threshold, no_progress_ticks, enabled
	FROM projects WHERE name = ?`, name).
		Scan(&cd, &floor, &ceiling, &threshold, &streak, &enabled)
	if err != nil {
		t.Fatalf("read adaptive state for %s: %v", name, err)
	}
	return
}

// reg002RepoRoot locates the repository root from this test file's own path.
func reg002RepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the repo root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/scheduler/x_test.go -> repo root
}

// reg002CountMatches counts, per non-test .go file under internal/ and cmd/,
// the lines matching pattern. Line-based so a scan can distinguish a call site
// from the function definition itself.
func reg002CountMatches(t *testing.T, pattern *regexp.Regexp) map[string]int {
	t.Helper()
	out := map[string]int{}
	root := reg002RepoRoot(t)
	for _, top := range []string{"internal", "cmd"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			n := 0
			for _, line := range strings.Split(string(b), "\n") {
				if pattern.MatchString(line) {
					n++
				}
			}
			if n > 0 {
				rel, _ := filepath.Rel(root, path)
				out[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// (2) An operator park is never escalated below the value in force at enable.
// --------------------------------------------------------------------------

// TestREG002_OperatorCooldownNeverEscalatedBelowInForce pins the floorless-row
// semantic the source documents: when cooldown_floor_s is unset (0) AND no
// explicit ceiling is set — the hand-edited-SQL relic shape, since the enable
// path always snapshots a floor — the cooldown_s IN FORCE is the effective
// floor and nothing may rewrite it upward. "Parked is not abandoned": a park
// that the escalation path ratchets on its own is not a park at all.
//
// Positive form of the assertion (2): iteration over the no-progress streak is
// asserted MONOTONIC NON-DECREASING FROM THE IN-FORCE VALUE, i.e. the value can
// only ever stay or grow, never shrink below the operator's park, and for this
// floorless row it in fact never moves at all.
func TestREG002_OperatorCooldownNeverEscalatedBelowInForce(t *testing.T) {
	// Gate identity: the operator-cooldown guard on the LEGACY autoSlowdown
	// path already exists and must keep working; this test covers the
	// adaptive path that guard does not reach. If that guard is ever
	// removed, this test's premise ("the adaptive path is where an operator
	// park is at risk") is no longer the only line of defence and the
	// reviewer must reconcile the two.
	reg002AssertExistingTestExists(t, "internal/scheduler/slowdown_test.go",
		"func TestAutoSlowdown_Idle_OperatorSetNotEscalated(")

	const (
		inForce = 43200 // the operator's park, in force when the project was enabled
		name    = "reg002-floorless-park"
	)
	db := slowdownTestDB(t)
	// floor 0 + ceiling 0 = floorless row: the in-force cooldown_s is the
	// effective floor (see internal/database/models.go floor semantics and
	// the ceiling-resolution comment in adaptive_cooldown.go).
	reg002InsertArmedProject(t, db, name, inForce, 0, 0, 3, 0, 1)

	prev := inForce
	for tick := 1; tick <= 5; tick++ {
		if !adaptiveCooldown(db, name, "", noProgressOutcome(name)) {
			t.Fatalf("tick %d: adaptiveCooldown returned false for an adaptive-armed project", tick)
		}
		cd, _, _, _, streak, _ := reg002AdaptiveState(t, db, name)
		if cd < prev {
			t.Fatalf("tick %d: cooldown_s DECREASED %d → %d — an operator park must never be escalated below the value in force at enable (\"parked is not abandoned\")",
				tick, prev, cd)
		}
		if cd != inForce {
			t.Fatalf("tick %d: cooldown_s = %d, want %d — with the floor unset the in-force cooldown IS the effective floor, so a floorless operator park must not be escalated at all (streak=%d)",
				tick, cd, inForce, streak)
		}
		prev = cd
	}

	// The streak itself must have advanced — otherwise the loop above proved
	// nothing (the escalator would have been inert).
	if _, _, _, _, streak, _ := reg002AdaptiveState(t, db, name); streak != 5 {
		t.Fatalf("streak = %d after 5 no-progress ticks, want 5 — the escalator did not run, so the park assertions above are vacuous", streak)
	}

	// The speed-up path must respect the park too: a real code commit resets
	// the streak, but with no floor to snap back to it must leave the park
	// byte-identical rather than dropping it to a derived default.
	workdir := t.TempDir()
	initTickRepo(t, workdir)
	if err := os.WriteFile(filepath.Join(workdir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome := TickOutcome{Project: name, Status: TickCompleted, Commits: 1}
	outcome.Started = gitCommitFiles(t, workdir, []string{"main.go"}, "feat: real work")
	if !adaptiveCooldown(db, name, workdir, outcome) {
		t.Fatal("adaptiveCooldown returned false on the progress tick")
	}
	cd, _, _, _, streak, _ := reg002AdaptiveState(t, db, name)
	if streak != 0 {
		t.Errorf("streak = %d after a code commit, want 0 (progress must reset the streak)", streak)
	}
	if cd != inForce {
		t.Errorf("cooldown_s = %d after a progress reset, want %d — with no floor set there is nothing to snap back to, and the operator's park must survive the reset path untouched", cd, inForce)
	}
}

// reg002AssertExistingTestExists fails when a named existing test function is
// no longer present in a named file. Used to gate assumptions these tests make
// about adjacent coverage — a renamed or deleted neighbour must break this test
// loudly, not silently invalidate it.
func reg002AssertExistingTestExists(t *testing.T, relPath, signature string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(reg002RepoRoot(t), relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	if !strings.Contains(string(b), signature) {
		t.Errorf("%s no longer contains %q — REG-002 assumptions about adjacent coverage must be re-checked", relPath, signature)
	}
}

// --------------------------------------------------------------------------
// (3) A disabled project never escalates.
// --------------------------------------------------------------------------

// TestREG002_DisabledProjectNeverEscalates pins "disabled projects never
// escalate" against the mechanism that actually enforces it, which is NOT a
// guard inside the escalator: adaptive_cooldown.go reads no `enabled` column at
// all. The gate is ADMISSION — a disabled row never comes out of the packer, so
// it never spawns, never completes a tick, and therefore never reaches the
// post-tick hook that owns escalation. This test pins both halves of that
// chain:
//
//	arm 1 (admission)   — a disabled, adaptive-armed, maximally overdue project
//	                      is not selected, while an enabled twin is;
//	arm 2 (reachability) — escalation is reachable ONLY from a tick-completion
//	                      hook, so no evaluation/startup path can escalate a
//	                      disabled row behind the packer's back.
//
// Residual, reported not fixed: because the escalator itself is not
// enabled-gated, a tick that is already RUNNING when its project is paused does
// still advance the streak and can escalate cooldown_s. That is a separate
// concern from this row (it requires a live tick), and the board row's rule is
// to report a real production defect rather than patch production code inside a
// regression-test change.
func TestREG002_DisabledProjectNeverEscalates(t *testing.T) {
	db := newTestDB(t)

	const control, disabled = "reg002-enabled-twin", "reg002-disabled-park"
	// Identical policy on both arms so the ONLY difference is `enabled`.
	reg002InsertArmedProject(t, db, control, 3600, 3600, 28800, 3, 3, 1)
	reg002InsertArmedProject(t, db, disabled, 3600, 3600, 28800, 3, 3, 0)
	// Both are maximally overdue: 48h since the last completed tick, far past
	// any cooldown — the GAP-011 force-select regime, which is the state in
	// which a forgotten disabled row is most likely to be dragged back in.
	for _, name := range []string{control, disabled} {
		if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = ?`,
			time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339), name); err != nil {
			t.Fatalf("age %s: %v", name, err)
		}
	}

	before := map[string][6]int{}
	for _, name := range []string{control, disabled} {
		cd, fl, ce, th, st, en := reg002AdaptiveState(t, db, name)
		before[name] = [6]int{cd, fl, ce, th, st, en}
	}

	p := NewPacker(db, NewUrgencyCalculator(time.Minute, time.Hour, 10), 100, 10, nil)
	got, err := p.Pick(time.Now(), nil)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}

	// arm 1 — the control proves the harness can see an escalatable project,
	// so the disabled arm's absence is evidence about the gate, not about the
	// fixture.
	picked := map[string]bool{}
	for _, gp := range got {
		picked[gp.Name] = true
	}
	if !picked[control] {
		t.Fatalf("admission arm is vacuous: the enabled, overdue control %q was not picked (picked=%v) — the disabled arm proves nothing about the enabled gate if the packer sees nothing at all", control, picked)
	}
	if picked[disabled] {
		t.Errorf("disabled project %q was admitted while an adaptive escalation was pending — a disabled project must never reach a spawn, so it can never reach the post-tick escalator (picked=%v)", disabled, picked)
	}

	// The admission attempt must be read-only with respect to the park: a
	// picker that rewrote cooldown/streak while merely LOOKING at a row would
	// escalate a disabled project without ever spawning it.
	for _, name := range []string{control, disabled} {
		cd, fl, ce, th, st, en := reg002AdaptiveState(t, db, name)
		if after := [6]int{cd, fl, ce, th, st, en}; after != before[name] {
			t.Errorf("selection mutated the park of %q\n before: %v\n after:  %v", name, before[name], after)
		}
	}

	// arm 2 — escalation must remain tick-completion-only. A new call site in
	// the evaluation loop, a startup sweep, or an API handler would be able to
	// escalate a row the packer never admitted.
	t.Run("escalation is reachable only from a tick-completion hook", func(t *testing.T) {
		// A CALL site passes a *sql.DB as its first argument; the definition
		// in adaptive_cooldown.go does not. Excluding the definition is what
		// makes this a call-site inventory rather than a name count.
		sites := reg002CountMatches(t, regexp.MustCompile(`adaptiveCooldown\((db|s\.db),`))
		var files []string
		for f := range sites {
			files = append(files, f)
		}
		sort.Strings(files)
		want := []string{"internal/scheduler/sim_spawn.go", "internal/scheduler/slot_pool.go"}
		if len(files) != len(want) {
			t.Fatalf("adaptiveCooldown call sites = %v, want exactly %v (both are post-tick completion hooks) — a new call site can escalate a project the packer never admitted, e.g. a disabled one", files, want)
		}
		for i, f := range files {
			if f != want[i] {
				t.Fatalf("adaptiveCooldown call sites = %v, want %v", files, want)
			}
		}
		// Pin the enclosing hook in each file: the call must sit in the
		// post-tick completion path, not in some other function.
		b, err := os.ReadFile(filepath.Join(reg002RepoRoot(t), "internal/scheduler/slot_pool.go"))
		if err != nil {
			t.Fatalf("read slot_pool.go: %v", err)
		}
		if !strings.Contains(string(b), "if !adaptiveCooldown(db, outcome.Project, proj.Workdir, outcome) {") {
			t.Error("slot_pool.go no longer gates the legacy autoSlowdown fallback on adaptiveCooldown — the tick-completion wiring REG-002 relies on has moved")
		}
	})
}

// --------------------------------------------------------------------------
// (4) Escalation state resets cleanly on re-enable.
// --------------------------------------------------------------------------

// TestREG002_EscalationStateResetsOnReEnable pins the ACTUAL re-enable
// semantics in two phases, because the source distinguishes two different
// meanings of "re-enable" and the board row's wording collapses them:
//
//	phase A — the operator's resume (POST /api/v1/projects/{name}/resume, i.e.
//	          PUT Enabled=true) is a BARE enable toggle. Source fact: it
//	          deliberately PRESERVES the escalation (streak and escalated
//	          cooldown_s both survive). "Parked is not abandoned" cuts both
//	          ways — a pause/resume must not silently un-park an escalated
//	          cooldown the operator never touched.
//	phase B — the adaptive feature re-arm (adaptive_cooldown false→true) is the
//	          documented clean-slate transition: streak 0, both board
//	          baselines reset, and the policy row normalized (floor snapshotted
//	          from the cooldown_s in force, ceiling = 8 × that floor, threshold
//	          back to the built-in default).
//
// Where the row says "escalation state resets cleanly on re-enable", the source
// resets it on the ADAPTIVE re-arm, not on the bare enable toggle; phase A
// asserts the actual behaviour and that difference is called out in the change
// report rather than papered over.
func TestREG002_EscalationStateResetsOnReEnable(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	const name = "reg002-reenable"

	// Escalated to the derived cap: floor 3600, ceiling 8 × floor = 28800,
	// streak at the threshold.
	const floorS, ceilingS, thresholdS = 3600, 28800, 3
	reg002InsertArmedProject(t, db, name, ceilingS, floorS, ceilingS, thresholdS, thresholdS, 1)

	// --- phase A: operator pause → plain resume (the /resume path shape) ----
	if err := UpdateProjectReg002(ctx, db, name, false); err != nil {
		t.Fatalf("pause: %v", err)
	}
	cd, _, _, _, streak, enabled := reg002AdaptiveState(t, db, name)
	if enabled != 0 {
		t.Fatalf("pause did not land: enabled = %d", enabled)
	}
	if cd != ceilingS || streak != thresholdS {
		t.Fatalf("pause mutated the escalation: cd=%d streak=%d, want %d/%d — a pause must leave the escalation exactly as it found it",
			cd, streak, ceilingS, thresholdS)
	}
	if err := UpdateProjectReg002(ctx, db, name, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	cd, _, _, _, streak, enabled = reg002AdaptiveState(t, db, name)
	if enabled != 1 {
		t.Fatalf("resume did not land: enabled = %d", enabled)
	}
	if streak != thresholdS || cd != ceilingS {
		t.Errorf("plain resume (the /resume path) reset the escalation: cd=%d streak=%d, want %d/%d — source fact: a bare enable toggle deliberately preserves the escalation, so phase A pins PRESERVATION, not reset", cd, streak, ceilingS, thresholdS)
	}

	// --- phase B: adaptive re-arm — the documented clean-slate transition ----
	// Turn the feature off first (leaves policy + streak untouched), which is
	// the state from which the false→true transition normalization runs.
	if err := UpdateProjectAdaptiveReg002(ctx, db, name, false); err != nil {
		t.Fatalf("adaptive off: %v", err)
	}
	var streakBeforeReArm int
	if _, _, _, _, streakBeforeReArm, _ = reg002AdaptiveState(t, db, name); streakBeforeReArm == 0 {
		t.Fatalf("streak = 0 before the re-arm, so the clean-slate assertion would be vacuous (turning the feature off must leave the streak alone)")
	}
	// Re-arm with the escalated cooldown still in force — the documented
	// snapshot semantic then adopts it as the new floor (and derives 8 × it).
	if err := UpdateProjectAdaptiveReg002(ctx, db, name, true); err != nil {
		t.Fatalf("adaptive re-arm: %v", err)
	}
	cd, fl, ce, th, streak, enabled := reg002AdaptiveState(t, db, name)
	if streak != 0 {
		t.Errorf("no_progress_ticks = %d after the adaptive re-arm, want 0 — escalation state must start from a clean slate (\"parked is not abandoned\": the next escalation begins at the floor, not mid-streak)", streak)
	}
	if cd != ceilingS {
		t.Errorf("cooldown_s = %d after the adaptive re-arm, want %d (the value in force at re-enable)", cd, ceilingS)
	}
	if fl != cd {
		t.Errorf("cooldown_floor_s = %d after the adaptive re-arm, want %d — the floor is the cooldown_s in force at enable time, so the next reset has a durable base", fl, cd)
	}
	if ce != fl*8 {
		t.Errorf("cooldown_ceiling_s = %d after the adaptive re-arm, want %d (8 × floor)", ce, fl*8)
	}
	if th != database.DefaultAdaptiveCooldownThreshold {
		t.Errorf("no_progress_threshold = %d after the adaptive re-arm, want %d (the re-arm normalizes the policy row back to the built-in default, not the pre-arm override of 3)",
			th, database.DefaultAdaptiveCooldownThreshold)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d after the adaptive re-arm, want 1", enabled)
	}

	// Both board baselines must be reset, or the escalator measures its next
	// streak against a stale prior era.
	var rowsSeen, openSeen int
	if err := db.QueryRow(`SELECT board_rows_seen, COALESCE(board_open_seen, -1) FROM projects WHERE name = ?`, name).
		Scan(&rowsSeen, &openSeen); err != nil {
		t.Fatalf("read board baselines: %v", err)
	}
	if rowsSeen != -1 || openSeen != -1 {
		t.Errorf("board baselines after the adaptive re-arm = (rows=%d, open=%d), want (-1, -1) — a stale baseline would let the next streak be measured against the previous era's board", rowsSeen, openSeen)
	}
}

// UpdateProjectReg002 is the bare enable toggle — the same ProjectUpdates shape
// POST /api/v1/projects/{name}/pause and /resume send (Enabled only, via
// database.UpdateProject; that identity is pinned in
// internal/database/reg002_park_persistence_test.go).
func UpdateProjectReg002(ctx context.Context, db *sql.DB, name string, enabled bool) error {
	return database.UpdateProject(ctx, db, name, database.ProjectUpdates{Enabled: database.BoolPtr(enabled)})
}

// UpdateProjectAdaptiveReg002 is the adaptive flag transition — the shape the
// fleet.toml loader sends on every boot (AdaptiveCooldown set; see
// internal/config/loader.go, which passes AdaptiveCooldown + the resolved
// policy numbers through database.UpdateProject).
func UpdateProjectAdaptiveReg002(ctx context.Context, db *sql.DB, name string, adaptive bool) error {
	return database.UpdateProject(ctx, db, name, database.ProjectUpdates{AdaptiveCooldown: database.BoolPtr(adaptive)})
}
