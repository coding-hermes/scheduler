package api

// PERF-003 — SQL-side truncation of the computeProjectFailureRates row set.
//
// The pre-PERF-003 SELECT (SCHED-PERF-002 shape) materialised every
// completed tick of every existing project (live DB 2026-11-05: 84,776
// rows) while the Go walk kept only the newest `window` per project
// (20,220 rows used) — a 4.2x over-fetch. Three proofs live here:
//
//  1. TestComputeProjectFailureRates_TruncatesInSQL — a scanned-row census:
//     the SELECT must materialise sum(min(completed_ticks, window)) rows
//     across real projects and nothing else (the assertion reads the
//     failureRateScannedRowCount seam, so it is on the SCANNED population,
//     not on the result-map size), and a ghost project's ticks must not be
//     scanned at all (DOGFOOD-009).
//  2. TestComputeProjectFailureRates_Equivalence — the frozen shipped
//     query+walk and the new bounds-CTE query+walk return reflect.DeepEqual
//     maps on a fixture with a short-history project, a harness-class row
//     (SCHED-GAP-173), an operator-drain reap (SCHED-GAP-1608), an
//     uncompleted tick and a ghost project.
//  3. BenchmarkComputeProjectFailureRates_Paired — both arms in ONE
//     -bench invocation over the SAME frozen live-shaped fixture, gated on
//     the medians: sql_truncated median ns/op <= 0.6 x shipped median ns/op.

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// perf003LegacyComputeFailureRates is a FROZEN copy of
// computeProjectFailureRates as it shipped before PERF-003: the
// SCHED-PERF-002 single-query shape (every completed tick of every existing
// project in one SELECT, ordered project_name ASC / spawned_at DESC) with
// the defensive per-project window in Go. It exists ONLY as the oracle for
// TestComputeProjectFailureRates_Equivalence and as the shipped arm of
// BenchmarkComputeProjectFailureRates_Paired.
//
// Do not modernise it — any drift here silently weakens the equivalence
// pin. The failureRateQueryCount.Add(1) seam bump is deliberately omitted:
// the oracle measures output equivalence, not round trips (OneRoundTrip
// owns that invariant).
func perf003LegacyComputeFailureRates(ctx context.Context, db *sql.DB, window int, threshold float64, minTicks int) map[string]ProjectFailureRate {
	if window <= 0 {
		window = 100
	}
	if minTicks <= 0 {
		minTicks = 50
	}
	out := map[string]ProjectFailureRate{}

	rows, err := db.QueryContext(ctx,
		`SELECT t.project_name, t.status, COALESCE(t.error, ''),
		        COALESCE(t.orphan_reason, '')
		 FROM ticks t
		 WHERE t.completed_at IS NOT NULL
		   AND EXISTS (SELECT 1 FROM projects p WHERE p.name = t.project_name)
		 ORDER BY t.project_name ASC, t.spawned_at DESC`)
	if err != nil {
		return out
	}
	defer rows.Close()

	var (
		curName    string
		curSeen    int // ticks observed for curName (capped at window)
		curFailed  int
		curTotal   int
		hasCurrent bool
	)
	flush := func() {
		if !hasCurrent {
			return
		}
		rate := 0.0
		if curTotal > 0 {
			rate = float64(curFailed) / float64(curTotal)
		}
		armed := threshold > 0 && curTotal >= minTicks && rate >= threshold
		rate = float64(int(rate*10000)) / 10000
		out[curName] = ProjectFailureRate{
			Failed:           curFailed,
			Total:            curTotal,
			FailureRate:      rate,
			AutoDisableArmed: armed,
		}
	}

	for rows.Next() {
		var name, status, errText, orphanReason string
		if err := rows.Scan(&name, &status, &errText, &orphanReason); err != nil {
			continue
		}
		if hasCurrent && name != curName {
			flush()
			curName = name
			curSeen, curFailed, curTotal = 0, 0, 0
		} else if !hasCurrent {
			curName = name
			hasCurrent = true
		}
		if curSeen >= window {
			continue
		}
		curSeen++
		if !scheduler.FailureIsLaneAttributableT(status, errText, orphanReason) {
			continue
		}
		curTotal++
		if status == "failed" || status == "timeout" {
			curFailed++
		}
	}
	flush()
	return out
}

// perf003StatusFor gives fixtures a deterministic status mix: every 4th
// tick failed, the rest completed.
func perf003StatusFor(i int) string {
	if i%4 == 0 {
		return "failed"
	}
	return "completed"
}

// TestComputeProjectFailureRates_TruncatesInSQL — the scanned-row census.
// Fixture: (a) one project holding MORE than window completed ticks,
// (b) one holding EXACTLY window, (c) one holding 2, (d) a ghost project
// (ticks with no projects row, via the purge path). The rows reaching the
// Go walk must equal sum over real projects of min(completed_ticks, window)
// — 10+10+2 = 22 here; the shipped pre-PERF-003 query scanned 35 (20+10+2
// real plus 3 ghost rows). The ghost project stays absent from the map.
func TestComputeProjectFailureRates_TruncatesInSQL(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	const window = 10

	// (a) more than the window: 20 completed ticks -> keep the newest 10.
	mustCreateHelperTestProject(t, db, "trunc-big")
	// (b) exactly the window: 10 completed ticks -> keep all 10.
	mustCreateHelperTestProject(t, db, "trunc-exact")
	// (c) fewer than the window: 2 completed ticks -> keep both (the
	// COALESCE('', cutoff) arm — a NULL cutoff would drop this project).
	mustCreateHelperTestProject(t, db, "trunc-small")

	now := time.Now()
	for i := 0; i < 20; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("trunc-big-%02d", i), "trunc-big",
			perf003StatusFor(i), now.Add(-time.Duration(40-i)*time.Minute))
	}
	for i := 0; i < 10; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("trunc-exact-%02d", i), "trunc-exact",
			perf003StatusFor(i), now.Add(-time.Duration(20-i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("trunc-small-%02d", i), "trunc-small",
			perf003StatusFor(i), now.Add(-time.Duration(5-i)*time.Minute))
	}

	// (d) ghost: ticks whose project row is purged (DOGFOOD-009 path).
	mustCreateHelperTestProject(t, db, "trunc-ghost")
	for i := 0; i < 3; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("trunc-ghost-%d", i), "trunc-ghost", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
	}
	if err := database.PurgeProject(ctx, db, "trunc-ghost"); err != nil {
		t.Fatalf("PurgeProject trunc-ghost: %v", err)
	}

	resetFailureRateScannedRowCountForTest()
	rates := computeProjectFailureRates(ctx, db, window, 0, 0)
	scanned := failureRateScannedRowCountForTest()

	// The load-bearing census assertion: on the SCANNED count, not the
	// map size. 10 + 10 + 2 — one window for (a), the full window for
	// (b), the short history for (c); zero rows for the ghost.
	wantScanned := 10 + 10 + 2
	if scanned != int64(wantScanned) {
		t.Fatalf("scanned rows = %d, want %d (SQL must materialise only "+
			"sum(min(completed_ticks, window)) across real projects; the shipped "+
			"query scanned all 35 completed ticks including the ghost's 3)",
			scanned, wantScanned)
	}
	if _, ok := rates["trunc-ghost"]; ok {
		t.Fatalf("ghost project present in failure rates: %+v", rates["trunc-ghost"])
	}
	if len(rates) != 3 {
		t.Fatalf("rates length = %d, want 3 (the three real projects): %+v", len(rates), rates)
	}

	// Non-vacuity: the kept window rows are the NEWEST ones. trunc-big
	// kept i in [10..19]; perf003StatusFor marks i%4==0 failed there
	// (i=12, i=16) -> total 10, failed 2, rate 0.2. A cutoff bug that
	// kept the OLDEST 10 (i in [0..9]) would show 3 failed instead.
	big := rates["trunc-big"]
	if big.Total != 10 || big.Failed != 2 || big.FailureRate != 0.2 {
		t.Errorf("trunc-big = %+v, want total=10 failed=2 rate=0.2 (newest 10 of 20)", big)
	}
	exact := rates["trunc-exact"]
	if exact.Total != 10 || exact.Failed != 3 {
		t.Errorf("trunc-exact = %+v, want total=10 failed=3 (short history kept whole)", exact)
	}
	small := rates["trunc-small"]
	if small.Total != 2 || small.Failed != 1 {
		t.Errorf("trunc-small = %+v, want total=2 failed=1 (fewer-than-window project kept)", small)
	}
}

// TestComputeProjectFailureRates_Equivalence — the frozen shipped arm and
// the new bounds-CTE arm must return reflect.DeepEqual maps on a real-shaped
// fixture: a project with fewer than window ticks (the NULL-cutoff arm), a
// project with more (truncation active), harness-class excluded rows
// (SCHED-GAP-173), operator-drain reaps (SCHED-GAP-1608), an uncompleted
// tick and a ghost project (DOGFOOD-009).
func TestComputeProjectFailureRates_Equivalence(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	const window = 10
	minute := func(n int) time.Time { return time.Now().Add(-time.Duration(n) * time.Minute) }

	// eq-few: 7 completed ticks — fewer than the window. The COALESCE('')
	// cutoff must keep all 7 (a plain NULL cutoff returned only the
	// >= window projects).
	mustCreateHelperTestProject(t, db, "eq-few")
	for i := 0; i < 7; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("eq-few-%02d", i), "eq-few",
			perf003StatusFor(i), minute(30-i))
	}

	// eq-many: 15 completed ticks (truncation active) + 1 uncompleted
	// (completed_at NULL) tick that neither arm may count.
	mustCreateHelperTestProject(t, db, "eq-many")
	for i := 0; i < 15; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("eq-many-%02d", i), "eq-many",
			perf003StatusFor(i), minute(60-i))
	}
	ts := minute(0).Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		"eq-many-running", "eq-many", "running", ts, ts); err != nil {
		t.Fatalf("insert uncompleted tick: %v", err)
	}

	// eq-harness: every failure in the window is harness-class
	// (SCHED-GAP-173 marker) — the entry must survive with total=0.
	mustCreateHelperTestProject(t, db, "eq-harness")
	for i := 0; i < 3; i++ {
		gap1608InsertTick(t, db, fmt.Sprintf("eq-harness-%d", i), "eq-harness",
			"failed", "", "gateway unreachable: 503", minute(20-i))
	}
	insertHelperTestTick(t, db, "eq-harness-ok", "eq-harness", "completed", minute(1))

	// eq-reap: three operator-reap shapes (SCHED-GAP-1608 — drain_timeout,
	// operator_restart, and the legacy graceful-shutdown error text) plus
	// two real completed ticks. Classification stays in Go.
	mustCreateHelperTestProject(t, db, "eq-reap")
	gap1608InsertTick(t, db, "eq-reap-drain", "eq-reap", "failed", "drain_timeout", "", minute(15))
	gap1608InsertTick(t, db, "eq-reap-restart", "eq-reap", "failed", "operator_restart", "", minute(14))
	gap1608InsertTick(t, db, "eq-reap-legacy", "eq-reap", "failed", "", gap1608AbortText, minute(13))
	insertHelperTestTick(t, db, "eq-reap-ok-1", "eq-reap", "completed", minute(2))
	insertHelperTestTick(t, db, "eq-reap-ok-2", "eq-reap", "completed", minute(1))

	// eq-ghost: purged project whose ticks remain (DOGFOOD-009).
	mustCreateHelperTestProject(t, db, "eq-ghost")
	for i := 0; i < 4; i++ {
		insertHelperTestTick(t, db, fmt.Sprintf("eq-ghost-%d", i), "eq-ghost", "failed",
			minute(12-i))
	}
	if err := database.PurgeProject(ctx, db, "eq-ghost"); err != nil {
		t.Fatalf("PurgeProject eq-ghost: %v", err)
	}

	// Same policy numbers on both arms (the live A/B ran threshold=0.9,
	// minTicks=50; a lower threshold here exercises the armed flag too).
	oldMap := perf003LegacyComputeFailureRates(ctx, db, window, 0.5, 5)
	newMap := computeProjectFailureRates(ctx, db, window, 0.5, 5)

	if !reflect.DeepEqual(oldMap, newMap) {
		t.Fatalf("arms disagree:\n  shipped (pre-PERF-003): %+v\n  sql_truncated:          %+v",
			oldMap, newMap)
	}

	// Non-vacuity pins (they pin both arms — DeepEqual passed above).
	if len(newMap) != 4 {
		t.Fatalf("rates length = %d, want 4 (eq-few, eq-many, eq-harness, eq-reap): %+v",
			len(newMap), newMap)
	}
	if _, ok := newMap["eq-ghost"]; ok {
		t.Fatalf("ghost project present in failure rates: %+v", newMap["eq-ghost"])
	}
	if got := newMap["eq-few"]; got.Total != 7 || got.Failed != 2 {
		t.Errorf("eq-few = %+v, want total=7 failed=2 (shorter-than-window project kept whole)", got)
	}
	// eq-many kept the newest 10 (i in [5..14]; i%4==0 -> i=8,12 failed
	// -> 2 failed; the NULL-completed_at tick is excluded by both arms).
	if got := newMap["eq-many"]; got.Total != 10 || got.Failed != 2 {
		t.Errorf("eq-many = %+v, want total=10 failed=2 (window of 15, uncompleted excluded)", got)
	}
	if got := newMap["eq-harness"]; got.Total != 1 || got.Failed != 0 {
		t.Errorf("eq-harness = %+v, want total=1 failed=0 (harness-class rows excluded, the real completed tick counts)", got)
	}
	if got := newMap["eq-reap"]; got.Total != 2 || got.Failed != 0 {
		t.Errorf("eq-reap = %+v, want total=2 failed=0 (operator reaps excluded)", got)
	}
}

// BenchmarkComputeProjectFailureRates_Paired — both arms in ONE -bench
// invocation over the SAME frozen fixture, mirroring the live DB shape
// (2026-11-05: 771 projects, 84,776 completed ticks fetched, 20,220 used,
// window=100 — a few whale projects with long histories plus many
// short-history projects). Fixture: 300 projects x 50 completed ticks +
// 68 whale projects x 1000 = 83,000 rows fetched, 15,000 + 68*100 = 21,800
// used (3.8x reduction, close to the live 4.2x).
//
// Gate: sql_truncated median ns/op <= 0.6 x shipped median ns/op (the live
// paired A/B measured ratio 0.580: 251.4ms vs 433.9ms). Skipped in -short.
// Evidence invocation:
//
//	env -u SCHEDULER_OPERATOR_RESTART_APPROVED go test -run '^$' \
//	  -bench 'BenchmarkComputeProjectFailureRates_Paired' -benchtime=10x \
//	  -count=3 ./internal/api/
func BenchmarkComputeProjectFailureRates_Paired(b *testing.B) {
	if testing.Short() {
		b.Skip("paired wall-clock benchmark skipped in -short mode")
	}
	db, err := database.InitDB(":memory:")
	if err != nil {
		b.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	const (
		window     = 100
		nSmall     = 300
		smallTicks = 50
		nWhale     = 68
		whaleTicks = 1000
	)
	now := time.Now()
	for i := 0; i < nSmall; i++ {
		name := fmt.Sprintf("pb-small-%03d", i)
		perfCreateProject(b, db, name)
		for j := 0; j < smallTicks; j++ {
			perfInsertTick(b, db, fmt.Sprintf("%s-%04d", name, j), name,
				perf003StatusFor(j), now.Add(-time.Duration(smallTicks-j)*time.Minute))
		}
	}
	for i := 0; i < nWhale; i++ {
		name := fmt.Sprintf("pb-whale-%03d", i)
		perfCreateProject(b, db, name)
		for j := 0; j < whaleTicks; j++ {
			perfInsertTick(b, db, fmt.Sprintf("%s-%04d", name, j), name,
				perf003StatusFor(j), now.Add(-time.Duration(whaleTicks-j)*time.Minute))
		}
	}

	medianOf := func(durs []time.Duration) time.Duration {
		if len(durs) == 0 {
			return 0
		}
		sorted := append([]time.Duration(nil), durs...)
		sort.Slice(sorted, func(a, c int) bool { return sorted[a] < sorted[c] })
		if len(sorted)%2 == 1 {
			return sorted[len(sorted)/2]
		}
		return (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	runArm := func(b *testing.B, fn func() map[string]ProjectFailureRate) []time.Duration {
		// Warm the planner and caches before timing; the fixture is
		// frozen so both arms see identical rows.
		for i := 0; i < 2; i++ {
			_ = fn()
		}
		b.ReportAllocs()
		b.ResetTimer()
		durs := make([]time.Duration, 0, b.N)
		for i := 0; i < b.N; i++ {
			start := time.Now()
			_ = fn()
			durs = append(durs, time.Since(start))
		}
		b.StopTimer()
		return durs
	}

	var shippedDurs, truncDurs []time.Duration
	b.Run("shipped", func(b *testing.B) {
		shippedDurs = runArm(b, func() map[string]ProjectFailureRate {
			return perf003LegacyComputeFailureRates(ctx, db, window, 0.9, 50)
		})
	})
	b.Run("sql_truncated", func(b *testing.B) {
		truncDurs = runArm(b, func() map[string]ProjectFailureRate {
			return computeProjectFailureRates(ctx, db, window, 0.9, 50)
		})
	})

	shippedMedian := medianOf(shippedDurs)
	truncMedian := medianOf(truncDurs)
	if shippedMedian == 0 || truncMedian == 0 {
		b.Fatalf("paired medians empty: shipped=%v truncated=%v", shippedDurs, truncDurs)
	}
	ratio := float64(truncMedian) / float64(shippedMedian)
	b.Logf("PERF-003 paired medians, same frozen fixture (%d rows fetched -> %d used): "+
		"shipped=%v sql_truncated=%v ratio=%.3f (gate: sql_truncated <= 0.600 x shipped)",
		nSmall*smallTicks+nWhale*whaleTicks, nSmall*smallTicks+nWhale*window,
		shippedMedian, truncMedian, ratio)
	if truncMedian > time.Duration(0.6*float64(shippedMedian)) {
		b.Fatalf("PERF-003 gate failed: sql_truncated median %v > 0.6 x shipped median %v (ratio %.3f)",
			truncMedian, shippedMedian, ratio)
	}
}
