package api_test

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── SCHED-GAP-1636: the /api/v1/projects spend snapshot cache ─────────────
//
// The endpoint used to run the full ticks aggregate on every request. It now
// serves a snapshot younger than a short TTL (see internal/api/
// budget_spend_cache.go). These cases pin the four properties that make that
// safe for its consumers:
//
//	(1) the first read is real,
//	(2) a read inside the TTL reuses the snapshot (execution — this is the fix),
//	(3) the snapshot expires, so spend still converges on its own,
//	(4) a FAILED spend query is never stored, and the endpoint still fails open.
func TestSchedGap1636_ProjectsSpendSnapshotCache(t *testing.T) {
	a := newAPITestServer(t)
	const lane = "cache-lane"
	mustCreateAPITestProject(t, a.db, lane)

	dayStart := scheduler.UTCDayStart(time.Now())
	insertAPICostTick(t, a, "t-1636-1", lane, dayStart.Add(time.Hour), 1.00)

	spentDaily := func(what string, parsed map[string]interface{}) float64 {
		t.Helper()
		p := findPayloadProject(t, parsed, lane)
		v, ok := p["spent_daily_usd"].(float64)
		if !ok {
			t.Fatalf("%s: spent_daily_usd = %v, want a number (the budget enrichment is missing)", what, p["spent_daily_usd"])
		}
		return v
	}
	get := func(what string) float64 {
		t.Helper()
		code, parsed := a.do(t, "GET", "/api/v1/projects", nil)
		if code != 200 {
			t.Fatalf("%s: GET /api/v1/projects = %d, want 200", what, code)
		}
		return spentDaily(what, parsed)
	}

	// (1) cold read: the snapshot is loaded from the ticks table.
	if got := get("cold read"); got != 1.00 {
		t.Fatalf("cold read spent_daily_usd = %.2f, want 1.00", got)
	}

	// (2) a tick that lands AFTER the snapshot is not visible inside the TTL —
	// the second read is served from memory, which is the whole point of the
	// fix (no aggregate, no DB round trip).
	insertAPICostTick(t, a, "t-1636-2", lane, dayStart.Add(2*time.Hour), 2.00)
	if got := get("read inside the TTL"); got != 1.00 {
		t.Errorf("read inside the TTL spent_daily_usd = %.2f, want the cached 1.00 — the handler re-queried instead of reusing the snapshot", got)
	}

	// (3) the snapshot expires: spend catches up without any invalidation call.
	a.server.SetBudgetSpendCacheTTL(20 * time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if got := get("read after expiry"); got != 3.00 {
		t.Errorf("read after expiry spent_daily_usd = %.2f, want 3.00 — the snapshot never went stale", got)
	}

	// (4a) fail-open: with the spend query broken the endpoint still answers
	// 200 with plain project rows (no budget fields), never a 5xx.
	//
	// A third tick lands first so the truth (7.00) differs from the snapshot
	// (3.00) — that difference is what makes (4b) able to tell "the failure was
	// not cached" apart from "the old snapshot was reused".
	insertAPICostTick(t, a, "t-1636-3", lane, dayStart.Add(3*time.Hour), 4.00)
	a.server.SetBudgetSpendCacheTTL(20 * time.Millisecond)
	time.Sleep(40 * time.Millisecond) // let the 3.00 snapshot expire

	hidden := false
	setTicksHidden := func(hide bool) {
		t.Helper()
		if hide == hidden {
			return
		}
		from, to := "ticks", "ticks_hidden_1636"
		if !hide {
			from, to = to, from
		}
		if _, err := a.db.Exec(`ALTER TABLE ` + from + ` RENAME TO ` + to); err != nil {
			t.Fatalf("rename %s to %s: %v", from, to, err)
		}
		hidden = hide
	}
	setTicksHidden(true)
	t.Cleanup(func() { setTicksHidden(false) })

	code, parsed := a.do(t, "GET", "/api/v1/projects", nil)
	if code != 200 {
		t.Fatalf("with the spend query broken, GET /api/v1/projects = %d, want 200 (fail-open)", code)
	}
	if p := findPayloadProject(t, parsed, lane); p["spent_daily_usd"] != nil {
		t.Errorf("fail-open payload carries spent_daily_usd = %v, want the field absent", p["spent_daily_usd"])
	}

	// (4b) and the failure was NOT stored as a snapshot: the very next read
	// (still inside the TTL window) reports the real spend, not the stale
	// 3.00 the expired snapshot held and not the 0.00 an empty failure
	// snapshot would have produced.
	setTicksHidden(false)
	if got := get("read right after recovery"); got != 7.00 {
		t.Errorf("read right after recovery spent_daily_usd = %.2f, want 7.00 — a failed load was cached and masked the real spend", got)
	}
}

// TestSchedGap1636_SpendCacheDisabledByZeroTTL pins the disable knob: with the
// TTL pinned to zero every read re-queries, so an operator (or a test that
// needs exact spend) can opt the snapshot cache out entirely.
func TestSchedGap1636_SpendCacheDisabledByZeroTTL(t *testing.T) {
	a := newAPITestServer(t)
	a.server.SetBudgetSpendCacheTTL(0)

	const lane = "no-cache-lane"
	mustCreateAPITestProject(t, a.db, lane)
	dayStart := scheduler.UTCDayStart(time.Now())
	insertAPICostTick(t, a, "t-1636-n1", lane, dayStart.Add(time.Hour), 1.50)

	readDaily := func(what string, want float64) {
		t.Helper()
		code, parsed := a.do(t, "GET", "/api/v1/projects", nil)
		if code != 200 {
			t.Fatalf("%s: GET /api/v1/projects = %d, want 200", what, code)
		}
		got, ok := findPayloadProject(t, parsed, lane)["spent_daily_usd"].(float64)
		if !ok || got != want {
			t.Errorf("%s: spent_daily_usd = %v, want %.2f", what, got, want)
		}
	}
	readDaily("first read", 1.50)
	insertAPICostTick(t, a, "t-1636-n2", lane, dayStart.Add(2*time.Hour), 2.50)
	// No TTL window is armed, so the second read must already see the new tick.
	readDaily("second read", 4.00)
}
