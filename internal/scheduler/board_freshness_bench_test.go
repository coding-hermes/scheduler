package scheduler

// ── PERF-002-R2: the mis-attribution scan is indexed, not quadratic ──────
//
// classifyCompleteRow (board_freshness.go) used to answer "does this commit
// name a DIFFERENT board row?" by running every board id's compiled
// `\bid\b` regex over the commit message:
//
//	for _, other := range ids { matcher(other).MatchString(msg) }
//
// a rows x ids product of regex calls. At fleet board size (656 rows x 656
// ids = 400,160 MatchString calls) that scan burned 4.60s of a 10.97s CPU
// sample window — 41.9% of ALL daemon CPU — on a read that must stay well
// inside a tick. The compilation was already amortized; the CALL COUNT was
// the cost.
//
// The fix indexes the ids once per read (rowIDIndex) and probes only the
// ids a message can actually name. These tests pin that:
//
//   - TestBoardFreshnessMisattributionScanEquivalence and
//     TestBoardFreshnessMisattributionScanFuzz differentially compare the
//     indexed scan against the PRE-FIX loop (exhaustiveNamesOtherID, kept
//     verbatim below as the oracle) over the repo's OWN board — the real id
//     corpus, real git commit messages, and adversarial boundary shapes.
//     The indexed scan must answer identically on every (message, row)
//     pair, because the row's correctness guarantee is the same one.
//   - TestBoardFreshnessMisattributionScanBudget is the recurrence catcher:
//     a full fleet-board sweep must stay under 100ms. The pre-fix scan took
//     ~5.1s at this size, so a recurrence — not host noise — is what fails.
//   - TestBoardFreshnessMisattributionScanProbeBudget pins the operation
//     count, which no slow host can mask: the scan must not go back to
//     probing every id.
//   - BenchmarkBoardFreshnessMisattributionScanFleetBoard measures the
//     indexed scan at fleet size; ...ExhaustiveReference measures the
//     pre-fix loop on the same corpus for the before/after contrast.

import (
	"fmt"
	"math/rand"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	// fleetScanBoardPath is the repo's own board — the production id
	// corpus (tests run with the package directory as CWD).
	fleetScanBoardPath = "../../.coding-hermes/board/tasks.jsonl"
	// fleetScanMinRows is the fleet board size the scan must survive.
	// The live board carried 656 rows when the regression was profiled.
	fleetScanMinRows = 600
	// misattributionScanBudget is the per-sweep wall-clock ceiling at
	// fleet size. The pre-fix scan measured ~5.1s here; the indexed scan
	// measures single-digit milliseconds, so the budget catches the
	// quadratic recurrence without being a host-speed test.
	misattributionScanBudget = 100 * time.Millisecond
)

// scanSink absorbs benchmark results so the compiler cannot delete the
// measured work.
var scanSink int

// fleetScanIDs returns the id corpus for the fleet-size gates: the repo's
// own board when it is present and fleet-sized, else a synthesized board so
// the gates still run on a checkout without it (fork PRs).
func fleetScanIDs(tb testing.TB) []string {
	tb.Helper()
	if ids := boardIDsFromFile(fleetScanBoardPath); len(ids) >= fleetScanMinRows {
		return ids
	}
	tb.Logf("board corpus %s has %d ids (<%d): using a synthetic fleet board",
		fleetScanBoardPath, len(boardIDsFromFile(fleetScanBoardPath)), fleetScanMinRows)
	return syntheticFleetIDs(fleetScanMinRows)
}

// boardIDsFromFile parses a JSONL board and returns its deduplicated ids in
// file order (the production read dedups by id; order is irrelevant to the
// scan).
func boardIDsFromFile(path string) []string {
	objs, _, err := parseBoardRowsFreshness(path)
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(objs))
	ids := make([]string, 0, len(objs))
	for _, obj := range objs {
		id := boardString(obj["id"])
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// syntheticFleetIDs builds a fleet-sized board whose id shapes mirror the
// live one (dash-joined alphanumeric families with shared prefixes).
func syntheticFleetIDs(n int) []string {
	families := []string{"SCHED-GAP", "PERF", "ADV-R", "QA-CHS", "DOC", "INT-CI", "DOGFOOD"}
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("%s-%03d-R%d", families[i%len(families)], i, i%3))
	}
	return ids
}

// exhaustiveNamesOtherID is the PRE-FIX implementation, kept verbatim as the
// differential oracle. Every board id's compiled regex is run over the
// message — the O(rows x ids) scan PERF-002-R2 replaced. It is the
// definition of "same correctness guarantee" the indexed scan must match.
func exhaustiveNamesOtherID(ids []string, matcher func(string) *regexp.Regexp, msg, self string) bool {
	for _, other := range ids {
		if other == self {
			continue
		}
		if m := matcher(other); m != nil && m.MatchString(msg) {
			return true
		}
	}
	return false
}

// scanProbeMessages returns adversarial messages built from the corpus ids:
// every boundary shape the `\bid\b` dialect has to get right. The oracle,
// not a hand-written expectation, decides the true answer.
func scanProbeMessages(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	a, b := ids[0], ids[len(ids)/2]
	last := ids[len(ids)-1]
	return []string{
		"", // empty message
		"chore: routine maintenance, no board row referenced", // names nothing
		"feat: index the scan. Addresses " + a + ".",          // trailing punctuation
		"fix: " + a + " and " + b,                             // two ids
		a + " " + b + " " + last,                              // three ids
		a + "-OLD",                                            // dash-suffixed alias: base id named
		a + "#3",                                              // hash-suffixed alias
		"(" + a + ")",                                         // parenthesised
		"[" + b + "] " + a,                                    // bracketed
		a + "X",                                               // longer token: NOT the id
		"x" + a,                                               // word char in front: NOT the id
		a + "_suffix",                                         // underscore is a word char: NOT the id
		strings.ToLower(a),                                    // case-sensitive: NOT the id
		a + "-" + b,                                           // id, dash, id
		a + "/" + b,                                           // slash-separated
		"QA: " + a + "; notes " + b + ", " + last,                              // punctuation salad
		strings.Repeat("padding ", 25) + a,                                     // long prologue
		"subject " + b + "\n\nbody names " + a + "\n\nAddresses " + last + ".", // multi-line body
		strings.Repeat(a+" ", 40),                                              // the same id many times
	}
}

// gitLogMessages returns up to n non-merge commit messages from the repo the
// test runs in — the real attribution surface this scan reads in production.
// A missing git history is not fatal: the synthetic probes still cover every
// boundary shape.
func gitLogMessages(tb testing.TB, n int) []string {
	tb.Helper()
	out, err := exec.Command("git", "log", "--no-merges", fmt.Sprintf("-n%d", n), "--format=%B%x1e").Output()
	if err != nil {
		tb.Logf("git log unavailable (%v): synthetic messages only", err)
		return nil
	}
	var msgs []string
	for _, rec := range strings.Split(string(out), "\x1e") {
		if rec = strings.TrimSpace(rec); rec != "" {
			msgs = append(msgs, rec)
		}
	}
	return msgs
}

// sampleIDs takes up to n evenly spaced ids (the whole corpus as "self"
// would be 600+ x 140 pairs; evenly spaced sampling keeps the differential
// test fast while still crossing every id family).
func sampleIDs(ids []string, n int) []string {
	if len(ids) <= n {
		return ids
	}
	step := len(ids) / n
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ids[i*step])
	}
	return out
}

// TestBoardFreshnessMisattributionScanEquivalence differentially compares the
// indexed scan against the pre-fix loop over the repo's own board: the real
// id corpus, real commit messages, and every adversarial boundary shape.
func TestBoardFreshnessMisattributionScanEquivalence(t *testing.T) {
	ids := fleetScanIDs(t)
	if len(ids) < fleetScanMinRows {
		t.Fatalf("corpus too small for the equivalence claim: %d ids", len(ids))
	}
	matcher := newIDMatcher()
	idx := newRowIDIndex(ids, matcher)

	// The pre-fix oracle runs every id's regex over every message, so the
	// arms below are sized to keep the differential honest AND the suite
	// fast: the boundary probes cheap, the real multi-KB commit bodies on
	// a few rows.
	selfs := sampleIDs(ids, 24)
	gitMsgs := gitLogMessages(t, 60)
	hits, pairs := 0, 0
	check := func(msg, self string) {
		t.Helper()
		want := exhaustiveNamesOtherID(ids, matcher, msg, self)
		got := idx.namesOtherID(msg, self)
		if got != want {
			t.Fatalf("indexed scan disagrees with the pre-fix loop:\n  ids=%d msg=%q self=%q\n  indexed=%v exhaustive=%v",
				len(ids), msg, self, got, want)
		}
		if want {
			hits++
		}
		pairs++
	}
	for _, msg := range scanProbeMessages(ids) {
		for _, self := range selfs {
			check(msg, self)
		}
	}
	// Real commit bodies (the production input) — a few rows each.
	for _, msg := range gitMsgs {
		for _, self := range selfs[:3] {
			check(msg, self)
		}
	}
	// Full fleet sweep against one boundary probe: every row, every id.
	probe := scanProbeMessages(ids)[5] // the dash-suffixed alias shape
	for _, self := range ids {
		check(probe, self)
	}
	longest := ""
	for _, msg := range append(append([]string{}, scanProbeMessages(ids)...), gitMsgs...) {
		if len(msg) > len(longest) {
			longest = msg
		}
	}
	for _, self := range selfs[:2] {
		check(longest, self)
	}
	if pairs < 1000 {
		t.Fatalf("equivalence coverage too thin: %d pairs", pairs)
	}
	if hits == 0 {
		t.Fatalf("equivalence arm is vacuous: none of the %d pairs reported a mis-attribution", pairs)
	}
	t.Logf("equivalence: %d (message, row) pairs over %d ids, %d mis-attribution hits, longest msg %d bytes",
		pairs, len(ids), hits, len(longest))
}

// TestBoardFreshnessMisattributionScanFuzz is the randomized arm of the
// differential test: messages assembled from id fragments, separators and
// case flips — the shapes a hand-written probe list misses. Seeded, so a
// failure reproduces exactly.
func TestBoardFreshnessMisattributionScanFuzz(t *testing.T) {
	ids := fleetScanIDs(t)
	matcher := newIDMatcher()
	idx := newRowIDIndex(ids, matcher)

	rng := rand.New(rand.NewSource(20261020))
	frag := []string{"", "-", "_", ".", ":", "#", "/", "(", ")", ",", " ", "\n", "X", "0", "a", "Z9", "Addresses"}
	pieces := func() string {
		var b strings.Builder
		for i := 0; i < 1+rng.Intn(8); i++ {
			switch rng.Intn(4) {
			case 0:
				id := ids[rng.Intn(len(ids))]
				lo, hi := rng.Intn(len(id)+1), rng.Intn(len(id)+1)
				if lo > hi {
					lo, hi = hi, lo
				}
				b.WriteString(id[lo:hi])
			case 1:
				b.WriteString(frag[rng.Intn(len(frag))])
			case 2:
				b.WriteString(strings.ToLower(frag[rng.Intn(len(frag))]))
			default:
				b.WriteString(ids[rng.Intn(len(ids))])
			}
		}
		return b.String()
	}

	pairs := 0
	for i := 0; i < 600; i++ {
		msg := pieces()
		self := ids[rng.Intn(len(ids))]
		want := exhaustiveNamesOtherID(ids, matcher, msg, self)
		if got := idx.namesOtherID(msg, self); got != want {
			t.Fatalf("fuzz disagreement at i=%d:\n  msg=%q\n  self=%q\n  indexed=%v exhaustive=%v", i, msg, self, got, want)
		}
		pairs++
	}
	// A board whose ids have non-word edges exercises the exhaustive
	// fallback list — mixed in so both arms are live in one read.
	weird := []string{"-lead", "trail-", "A#B", "PERF-1", "ok-id", "#5", "Q"}
	widx := newRowIDIndex(weird, matcher)
	// "-lead"/"trail-"/"#5" have a non-word edge and "Q" is too short for
	// the two-byte filter, so both classes take the exhaustive loop; the
	// rest — including "A#B", whose '#' is interior — are walked.
	if got := strings.Join(widx.edge, ","); got != "-lead,trail-,#5,Q" {
		t.Fatalf("exhaustive-fallback list = %q, want %q", got, "-lead,trail-,#5,Q")
	}
	if widx.maxLen != len("PERF-1") {
		t.Fatalf("maxLen = %d, want %d", widx.maxLen, len("PERF-1"))
	}
	wmsgs := append(scanProbeMessages(weird),
		"-lead", "trail-", "A#B", "A#BX", "-lead-", "x#5", "#5x", "ok-id.",
		"with -lead and PERF-1", "Q", " Q ", "Qx", "xQ", "Q, ok-id")
	for _, msg := range wmsgs {
		for _, self := range append(append([]string{}, weird...), "") {
			want := exhaustiveNamesOtherID(weird, matcher, msg, self)
			if got := widx.namesOtherID(msg, self); got != want {
				t.Fatalf("non-word-edged disagreement:\n  ids=%v msg=%q self=%q\n  indexed=%v exhaustive=%v", weird, msg, self, got, want)
			}
			pairs++
		}
	}
	t.Logf("fuzz: %d pairs (600 random + %d mixed-edge)", pairs, len(wmsgs)*len(weird))
	if pairs < 800 {
		t.Fatalf("fuzz coverage too thin: %d pairs", pairs)
	}
}

// TestBoardFreshnessMisattributionScanBudget is the recurrence catcher: one
// full fleet-board sweep of the mis-attribution scan must finish inside the
// budget. The pre-fix scan took ~5.1s at this size (656 rows x 656 ids =
// 400,160 MatchString calls), so the ~100ms budget fails loudly on a
// regression and never on host noise. Best of three rounds, because the
// fleet host is shared and a single round can be descheduled.
func TestBoardFreshnessMisattributionScanBudget(t *testing.T) {
	ids := fleetScanIDs(t)
	if len(ids) < fleetScanMinRows {
		t.Fatalf("corpus too small for a fleet-size budget claim: %d ids", len(ids))
	}
	idx := newRowIDIndex(ids, newIDMatcher())
	msgs := gitLogMessages(t, 60)
	longest := "chore: routine maintenance with no board row referenced at all"
	median := ""
	for _, msg := range msgs {
		if len(msg) > len(longest) {
			longest = msg
		}
		if median == "" || (len(msg) > len(median) && len(msg) <= 3000) {
			median = msg
		}
	}
	if median == "" {
		median = longest
	}

	arms := []struct {
		name string
		msg  string
	}{
		// Worst case for the pre-fix loop: nothing matches, so every id
		// regex runs — the exact product the profile caught.
		{"names-no-row", "chore: routine maintenance with no board row referenced at all"},
		// The production input shape: a real multi-KB commit body.
		{"real-median-message", median},
		// The longest message in the repo's history, applied to every row
		// — a harsher-than-production amplification of message size.
		{"longest-real-message", longest},
		// The common case: the message names exactly one row (not self).
		{"names-one-row", "feat: wire the index. Addresses " + ids[len(ids)-1] + "."},
	}
	for _, arm := range arms {
		best := time.Duration(0)
		for round := 0; round < 3; round++ {
			start := time.Now()
			hits := 0
			for _, self := range ids {
				if idx.namesOtherID(arm.msg, self) {
					hits++
				}
			}
			scanSink = hits
			if elapsed := time.Since(start); best == 0 || elapsed < best {
				best = elapsed
			}
		}
		t.Logf("scan budget arm %-20s rows=%d msg=%dB best=%v (budget %v)",
			arm.name, len(ids), len(arm.msg), best, misattributionScanBudget)
		if best > misattributionScanBudget {
			t.Fatalf("mis-attribution scan arm %q at %d rows took %v (budget %v) — the per-id quadratic scan is back",
				arm.name, len(ids), best, misattributionScanBudget)
		}
	}
}

// TestBoardFreshnessMisattributionScanProbeBudget pins the OPERATION count,
// which no loaded host can mask. The pre-fix scan probed one id per (row,
// id) pair — len(ids)^2 ≈ 450k at fleet size; the indexed scan probes only
// what the message names. This is the deterministic sibling of the
// wall-clock budget above.
func TestBoardFreshnessMisattributionScanProbeBudget(t *testing.T) {
	ids := fleetScanIDs(t)
	probes := 0
	base := newIDMatcher()
	counted := func(id string) *regexp.Regexp {
		probes++
		return base(id)
	}
	idx := newRowIDIndex(ids, counted)
	square := len(ids) * len(ids)

	// A message naming nothing: the scan must do ZERO id work — it cannot
	// know about an id the message does not contain.
	probes = 0
	for _, self := range ids {
		if idx.namesOtherID("chore: nothing here names a board row", self) {
			t.Fatalf("a message with no id reported a mis-attribution (self=%q)", self)
		}
	}
	if probes != 0 {
		t.Fatalf("scan probed %d ids for a message that names none (want 0)", probes)
	}

	// A message naming one row (the last, so a per-id loop would walk the
	// whole list to find it): at most one probe per row.
	named := ids[len(ids)-1]
	probes = 0
	hits := 0
	for _, self := range ids {
		if idx.namesOtherID("Addresses "+named+".", self) {
			hits++
		}
	}
	if hits != len(ids)-1 {
		t.Fatalf("mis-attribution hits = %d, want %d (every row but the named one)", hits, len(ids)-1)
	}
	if probes > len(ids)*2 {
		t.Fatalf("scan probed %d ids for a one-id message at %d rows (pre-fix would be %d): the per-id loop is back",
			probes, len(ids), square)
	}
	t.Logf("probe budget: %d rows, %d probes for a one-id message (pre-fix %d) — %.0fx fewer",
		len(ids), probes, square, float64(square)/float64(probes))
}

// benchScanMessages returns the message mix the benchmarks sweep: the
// no-match worst case plus the repo's real commit messages (cycling by row).
func benchScanMessages(tb testing.TB, ids []string) []string {
	msgs := []string{"feat: a commit body that names no board row at all, so every id must be probed"}
	msgs = append(msgs, gitLogMessages(tb, 200)...)
	if len(ids) > 0 {
		msgs = append(msgs, "fix: something. Addresses "+ids[len(ids)/2]+".")
	}
	return msgs
}

// BenchmarkBoardFreshnessMisattributionScanFleetBoard measures one full
// fleet-board sweep of the indexed scan — the production scan, at the size
// the regression was profiled on.
func BenchmarkBoardFreshnessMisattributionScanFleetBoard(b *testing.B) {
	ids := fleetScanIDs(b)
	idx := newRowIDIndex(ids, newIDMatcher())
	msgs := benchScanMessages(b, ids)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits := 0
		for j, self := range ids {
			if idx.namesOtherID(msgs[j%len(msgs)], self) {
				hits++
			}
		}
		scanSink = hits
	}
	b.ReportMetric(float64(len(ids)), "rows/sweep")
}

// BenchmarkBoardFreshnessMisattributionScanExhaustiveReference measures the
// PRE-FIX loop on the same corpus: the before/after contrast behind the
// budget. Expect one iteration to take seconds at fleet size.
func BenchmarkBoardFreshnessMisattributionScanExhaustiveReference(b *testing.B) {
	ids := fleetScanIDs(b)
	matcher := newIDMatcher()
	msgs := benchScanMessages(b, ids)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits := 0
		for j, self := range ids {
			if exhaustiveNamesOtherID(ids, matcher, msgs[j%len(msgs)], self) {
				hits++
			}
		}
		scanSink = hits
	}
	b.ReportMetric(float64(len(ids)), "rows/sweep")
}
