package scheduler

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestSumSessionCostInWindow verifies the real-cost lookup matches sessions
// whose activity overlaps the tick window, sums estimated (falling back from
// actual when actual is 0), and excludes out-of-window sessions.
func TestSumSessionCostInWindow(t *testing.T) {
	now := float64(time.Now().Unix())
	tmp := t.TempDir() + "/cost.db"
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE session_model_usage (
		session_id TEXT, model TEXT, billing_provider TEXT DEFAULT '',
		estimated_cost_usd REAL DEFAULT 0, actual_cost_usd REAL DEFAULT 0,
		first_seen REAL, last_seen REAL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO session_model_usage
		(session_id, model, estimated_cost_usd, actual_cost_usd, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?)`
	for _, r := range [][]any{
		{"in-window-est", "m", 0.10, 0, now - 100, now - 50},       // estimated only
		{"in-window-actual", "m", 0.20, 0.30, now - 100, now - 50}, // actual wins
		{"before-window", "m", 0.90, 0, now - 5000, now - 4900},    // excluded
		{"after-window", "m", 0.90, 0, now + 5000, now + 5100},     // excluded
	} {
		if _, err := db.Exec(ins, r...); err != nil {
			t.Fatalf("insert %v: %v", r[0], err)
		}
	}
	db.Close()

	start := time.Unix(int64(now-200), 0)
	end := time.Unix(int64(now-40), 0)

	cost, n, err := sumSessionCostInWindow(tmp, start, end)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// Expected: 0.10 (est) + 0.30 (actual wins) = 0.40, 2 sessions.
	if n != 2 {
		t.Errorf("expected 2 sessions, got %d", n)
	}
	if cost < 0.39 || cost > 0.41 {
		t.Errorf("expected ~0.40 cost, got %v", cost)
	}
}

// TestResolveRealTickCostFallback ensures a missing state.db falls back to the
// flat estimate rather than 0 or an error.
func TestResolveRealTickCostFallback(t *testing.T) {
	cost, isReal := resolveRealTickCost("/nonexistent/path", "/nonexistent/workdir", "proj",
		time.Now().Add(-time.Hour), time.Now())
	if isReal {
		t.Errorf("expected fallback (isReal=false), got isReal=true cost=%v", cost)
	}
	// Fallback should be the flat estimate ($0.032), not 0.
	if cost <= 0 {
		t.Errorf("expected non-zero fallback estimate, got %v", cost)
	}
}

// TestSumGitreinsUsageInWindow verifies the gitreins judge usage reader sums
// token cost for lines in the window and skips out-of-window/malformed lines.
func TestSumGitreinsUsageInWindow(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	p := dir + "/usage.jsonl"
	lines := []string{
		`{"ts": ` + fmt.Sprintf("%.0f", float64(now.Add(-time.Hour).Unix())) + `, "tokens_in": 10000, "tokens_out": 5000}`,
		`{"ts": ` + fmt.Sprintf("%.0f", float64(now.Add(-10*time.Minute).Unix())) + `, "tokens_in": 10000, "tokens_out": 5000}`,
		`{"ts": ` + fmt.Sprintf("%.0f", float64(now.Add(time.Hour).Unix())) + `, "tokens_in": 99999, "tokens_out": 99999}`, // out of window
		`not-json`, // malformed
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// tokens_in 10000*0.000002 + tokens_out 5000*0.000008 = 0.02 + 0.04 = 0.06 per in-window line
	cost, n, err := sumGitreinsUsageInWindow(p, now.Add(-2*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 in-window lines, got %d", n)
	}
	if cost < 0.119 || cost > 0.121 {
		t.Errorf("expected ~0.12 cost (2 lines x 0.06), got %v", cost)
	}
}
