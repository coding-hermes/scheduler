package scheduler

import (
	"bytes"
	"database/sql"
	"fmt"
	"testing"

	"github.com/coding-herms/scheduler/internal/database"
)

// newTestDB is duplicated from spawn_test.go — centralize later.
func slowdownTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// insertSlowdownProject inserts a project row with all required NOT NULL fields
// and a specific cooldown_s value.
func insertSlowdownProject(t *testing.T, db *sql.DB, name string, cooldown int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at)
		VALUES (?, ?, ?, 10, 5, ?, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', 1,
		        datetime('now'), datetime('now'))`,
		name, "https://github.com/example/"+name, "/tmp/work/"+name, cooldown,
	)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

// getSlowdownCooldown reads the current cooldown_s for a project from the DB.
func getSlowdownCooldown(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var cd int
	if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", name).Scan(&cd); err != nil {
		t.Fatalf("query cooldown_s for %s: %v", name, err)
	}
	return cd
}

// =============================================================================
// Nil / empty output tests
// =============================================================================

func TestAutoSlowdown_NilOutput(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "nilproj", 600)

	autoSlowdown(db, "nilproj", nil)

	if got := getSlowdownCooldown(t, db, "nilproj"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (unchanged for nil output)", got)
	}
}

func TestAutoSlowdown_EmptyOutput(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "emptyproj", 600)

	var buf bytes.Buffer
	autoSlowdown(db, "emptyproj", &buf)

	if got := getSlowdownCooldown(t, db, "emptyproj"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (unchanged for empty output)", got)
	}
}

// =============================================================================
// IDLE detection tests — all three keyword patterns
// =============================================================================

func TestAutoSlowdown_Idle_IdleTick(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "idle1", 600)

	var buf bytes.Buffer
	buf.WriteString("foreman tick output\nIDLE TICK — nothing to do\n")
	autoSlowdown(db, "idle1", &buf)

	if got := getSlowdownCooldown(t, db, "idle1"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (600 * 1.5)", got)
	}
}

func TestAutoSlowdown_Idle_SlowdownRequested(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "idle2", 600)

	var buf bytes.Buffer
	buf.WriteString("SLOWDOWN REQUESTED by foreman due to repeated idle cycles\n")
	autoSlowdown(db, "idle2", &buf)

	if got := getSlowdownCooldown(t, db, "idle2"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (600 * 1.5)", got)
	}
}

func TestAutoSlowdown_Idle_VerdictIdle(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "idle3", 600)

	var buf bytes.Buffer
	buf.WriteString("VERDICT: project is idle — IDLE\n")
	autoSlowdown(db, "idle3", &buf)

	if got := getSlowdownCooldown(t, db, "idle3"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (600 * 1.5)", got)
	}
}

// =============================================================================
// IDLE: cooldown escalation chain and cap
// =============================================================================

func TestAutoSlowdown_Idle_EscalationChain(t *testing.T) {
	tests := []struct {
		current int
		want    int
	}{
		{600, 900},   // 600 * 1.5 = 900
		{900, 1350},  // 900 * 1.5 = 1350
		{1350, 2025}, // 1350 * 1.5 = 2025
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d→%d", tt.current, tt.want), func(t *testing.T) {
			db := slowdownTestDB(t)
			name := fmt.Sprintf("escalate_%d", tt.current)
			insertSlowdownProject(t, db, name, tt.current)

			var buf bytes.Buffer
			buf.WriteString("IDLE TICK\n")
			autoSlowdown(db, name, &buf)

			if got := getSlowdownCooldown(t, db, name); got != tt.want {
				t.Errorf("cooldown = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAutoSlowdown_Idle_CapAt86400(t *testing.T) {
	tests := []struct {
		name    string
		current int
		want    int
	}{
		{"57600→86400 (exactly at cap)", 57600, 86400},
		{"86400→86400 (already capped, no write)", 86400, 86400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := slowdownTestDB(t)
			insertSlowdownProject(t, db, "cap_test", tt.current)

			var buf bytes.Buffer
			buf.WriteString("IDLE TICK\n")
			autoSlowdown(db, "cap_test", &buf)

			if got := getSlowdownCooldown(t, db, "cap_test"); got != tt.want {
				t.Errorf("cooldown = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestAutoSlowdown_Idle_Cooldown57600ToCapped verifies 57600→86400 exactly hits the cap.
func TestAutoSlowdown_Idle_Cooldown57600ToCapped(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "cap_57600", 57600)

	var buf bytes.Buffer
	buf.WriteString("IDLE TICK\n")
	autoSlowdown(db, "cap_57600", &buf)

	if got := getSlowdownCooldown(t, db, "cap_57600"); got != 86400 {
		t.Errorf("cooldown = %d, want 86400 (57600 * 1.5 = 86400, equals cap)", got)
	}
}

// TestAutoSlowdown_Idle_CooldownAlready86400_NoWrite verifies that when
// the cooldown is already 86400, it stays at 86400 (cap enforced, no DB write).
func TestAutoSlowdown_Idle_CooldownAlready86400_NoWrite(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "cap_86400", 86400)

	var buf bytes.Buffer
	buf.WriteString("IDLE TICK\n")
	autoSlowdown(db, "cap_86400", &buf)

	if got := getSlowdownCooldown(t, db, "cap_86400"); got != 86400 {
		t.Errorf("cooldown = %d, want 86400 (unchanged — already at cap)", got)
	}
}

// =============================================================================
// IDLE: zero cooldown treated as 600 → escalates to 900
// =============================================================================

func TestAutoSlowdown_Idle_ZeroCooldownDefaults(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "zeroCD", 0)

	var buf bytes.Buffer
	buf.WriteString("IDLE TICK\n")
	autoSlowdown(db, "zeroCD", &buf)

	if got := getSlowdownCooldown(t, db, "zeroCD"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (0 → 600 default → *1.5 = 900)", got)
	}
}

// =============================================================================
// PRODUCTIVE detection tests
// =============================================================================

func TestAutoSlowdown_Productive_ResetsElevated(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "prod_reset", 1350)

	var buf bytes.Buffer
	buf.WriteString("VERDICT: worked on feature — PRODUCTIVE\n")
	autoSlowdown(db, "prod_reset", &buf)

	if got := getSlowdownCooldown(t, db, "prod_reset"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (reset from elevated)", got)
	}
}

func TestAutoSlowdown_Productive_ResetsFromProductively(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "prod_reset2", 2025)

	var buf bytes.Buffer
	buf.WriteString("VERDICT: productively completed the task\n")
	autoSlowdown(db, "prod_reset2", &buf)

	if got := getSlowdownCooldown(t, db, "prod_reset2"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (reset from elevated via 'productively')", got)
	}
}

func TestAutoSlowdown_Productive_AlreadyAtBase_NoWrite(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "prod_base", 600)

	var buf bytes.Buffer
	buf.WriteString("VERDICT: PRODUCTIVE — already doing fine\n")
	autoSlowdown(db, "prod_base", &buf)

	if got := getSlowdownCooldown(t, db, "prod_base"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (unchanged — already at base)", got)
	}
}

// =============================================================================
// PRODUCTIVE overrides IDLE — isProductive checked only when NOT idle
// =============================================================================

func TestAutoSlowdown_IdleOverridesProductive(t *testing.T) {
	// When both IDLE and PRODUCTIVE keywords are present, IDLE wins because
	// isProductive is only evaluated when !isIdle.
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "idle_wins", 600)

	var buf bytes.Buffer
	// Contains both IDLE TICK and VERDICT: PRODUCTIVE — IDLE wins.
	buf.WriteString("IDLE TICK — VERDICT: PRODUCTIVE but we were idle\n")
	autoSlowdown(db, "idle_wins", &buf)

	if got := getSlowdownCooldown(t, db, "idle_wins"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (IDLE takes precedence — escalated)", got)
	}
}

func TestAutoSlowdown_ProductiveOnlyWhenNotIdle(t *testing.T) {
	// Productive keyword in output without IDLE triggers reset.
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "prod_only", 900)

	var buf bytes.Buffer
	buf.WriteString("VERDICT: PRODUCTIVE — made progress\n")
	autoSlowdown(db, "prod_only", &buf)

	if got := getSlowdownCooldown(t, db, "prod_only"); got != 600 {
		t.Errorf("cooldown = %d, want 600 (productive only, no idle → reset)", got)
	}
}

// =============================================================================
// Non-idle, non-productive output → no change
// =============================================================================

func TestAutoSlowdown_NeutralOutput_NoChange(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "neutral", 900)

	var buf bytes.Buffer
	buf.WriteString("foreman tick completed — worker ran but exited with error\n")
	autoSlowdown(db, "neutral", &buf)

	if got := getSlowdownCooldown(t, db, "neutral"); got != 900 {
		t.Errorf("cooldown = %d, want 900 (unchanged — neutral output)", got)
	}
}

// =============================================================================
// DB error handling
// =============================================================================

func TestAutoSlowdown_DBError_IdlePath(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "dberr", 600)
	db.Close() // close so QueryRow fails

	var buf bytes.Buffer
	buf.WriteString("IDLE TICK\n")

	// Should return silently — no panic.
	autoSlowdown(db, "dberr", &buf)
}

func TestAutoSlowdown_DBError_ProductivePath(t *testing.T) {
	db := slowdownTestDB(t)
	insertSlowdownProject(t, db, "dberr2", 1350)
	db.Close()

	var buf bytes.Buffer
	buf.WriteString("VERDICT: PRODUCTIVE\n")

	// Should return silently — no panic.
	autoSlowdown(db, "dberr2", &buf)
}
