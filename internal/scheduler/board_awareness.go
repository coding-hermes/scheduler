package scheduler

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// Board-aware pending-task urgency boost (SCHED-GAP-019, 2026-08-09).
//
// Problem: with the fleet saturated (maxConcurrent=4, all slots busy), a
// project whose board gains pending tasks can wait ~5h past eligibility before
// being picked. Selection is pure urgency + the S-GAP-001 starvation boost —
// nothing reads downstream project boards, so freshly-pending work has zero
// influence on selection order.
//
// Fix: PendingTaskCounter reads each project's board (tasks.jsonl or tasks.md)
// and caches the pending count. When a project has pending tasks, its urgency
// is boosted to pendingBoostUrgency + min(pending, 1000) — far above any
// organic urgency (~12k) but BELOW the starvation boost (1e12), so a starving
// project always outranks a pending one. The boost applies ONLY to projects
// already eligible (cooldown elapsed, not running) — cooldown checks are
// untouched (prepaid-bucket economics).

// Board-task selection model (GAP-036, documented 2026-08-13).
// Selection happens in TWO layers; conflating them makes this project's own
// oldest board rows look "ignored" when they are actually fixtures or gates:
//
// (a) PROJECT selection is the daemon's job (this package): Loop.evaluate
//     (tick_process.go) picks PROJECTS each eval via the multi-pool packer
//     (multipool_packer.go) or the flat fallback (packer.go/packer_select.go),
//     ordered by urgency = priority * (1 + elapsed/interval)^decayRate
//     (urgency.go) with three boost tiers — organic (~12k live), this pending
//     boost (5e11, SCHED-GAP-019), and the starvation boost (1e12, S-GAP-001).
//     Cooldown and the running set are hard gates; no boost bypasses cooldown.
//     The daemon never reads or selects individual board TASK rows.
//
// (b) TASK selection belongs to each project's FOREMAN: the spawn prompt
//     (spawn.go) tells the session to read ".coding-hermes/board/tasks.jsonl"
//     and execute one foreman tick per the foreman skill — the foreman picks
//     rows on priority/age/deps and writes status/attempts back. This repo's
//     own board is .coding-hermes/board/{tasks,events,fixtures,board}.jsonl
//     (git-tracked).
//
// (c) Fixture rows — the ids declared active in .coding-hermes/board/
//     fixtures.jsonl (E2E-001, NEVER-DONE, GITREINS-JUDGE) — are perpetual
//     recurring chores, not dispatchable tasks. ADV-R05 made that registry
//     the CANONICAL fixture representation: it is read by CountPending and
//     boardOpenRows, and a declared fixture is excluded BY DATA whatever
//     status its row carries (see fixture_registry.go). The foreman runs
//     them on cadence (E2E-001 light every tick, full battery every +5;
//     NEVER-DONE full 12-point every +3), so their rows legitimately stay
//     status=pending/attempts=0 forever. Run evidence is in each row's
//     worker_summary and in events.jsonl detail.fixtures (e.g. "E2E-001
//     FULL battery ran tick #344 ... next full due #349+");
//     pending/attempts=0 on a fixture is EXPECTED.
//
// (d) Blocked rows — status=blocked with blocked_reason set (e.g. FIX-STACK,
//     "Bane defers (systemd enable decision)") — are USER-GATED and stay
//     blocked until an operator unblocks; CountPending below deliberately
//     counts only status=="pending", never blocked rows.
//
// (e) The boost below affects ONLY (a): pending rows raise PROJECT urgency so
//     a project with fresh board work is picked sooner. It cannot dispatch,
//     unblock, or complete a task row. Fixture rows (SCHED-GAP-106, and by
//     registry declaration since ADV-R05) never count toward the pending
//     total — a project whose only open rows are fixtures gets NO boost.
//
// (f) Pending vs open (ADV-R05, rule stated): CountPending below counts
//     ONLY status=="pending" — the one status whose semantics are
//     "dispatchable now" fleet-wide (the foreman contract: recurring
//     fixtures stay pending, dispatchable work is filed pending). The
//     wider open vocabulary (todo, in_progress, claimed, ...) is
//     intentionally invisible to the pending boost: those statuses' work-
//     readiness varies by board (this repo's todo rows mix PM-filed
//     proposals and real bug reports), so counting them would guess
//     semantics and shift SCHED-GAP-065 idle-tick routing fleet-wide. The
//     open-work signal that DOES see the wider vocabulary is boardOpenRows
//     (adaptive_cooldown.go), which feeds adaptive cooldown, not selection.
//     This closes the "scheduler sees 15 pending vs human sees 34 open"
//     split from the ADV-R05 filing as INTENTIONAL: the two numbers answer
//     different questions (dispatchable vs open) and each is correct for
//     its own.

const (
	// pendingBoostUrgency is the BASE of the board-awareness boost — far
	// above any organically reachable urgency (live fleet tops out ~12k), so
	// a project with pending board tasks always sorts ahead of every ordinary
	// project. It is BELOW starvationBoostUrgency (1e12) so a starving project
	// always wins over a pending one — the S-GAP-001 starvation guarantee
	// must not be weakened.
	pendingBoostUrgency = 5e11

	// pendingBoostMaxCount caps the count term added to the boost so a board
	// with thousands of pending tasks does not inflate urgency unboundedly.
	pendingBoostMaxCount = 1000

	// bumpBoostUrgency is the SCHED-GAP-107 bump tier: an actively bumped
	// project runs at bump cooldown for N ticks and must actually get those
	// ticks, so it gets the same urgency class as the pending-work boost —
	// far above any organic urgency, below the starvation guarantee.
	// Slightly above pendingBoostUrgency so a bumped project wins ties
	// against other pending-boosted projects in the same eval.
	bumpBoostUrgency = pendingBoostUrgency + 1e6
)

// pendingBoostUrgencyFor returns the board-awareness-boosted urgency for a
// project with the given number of pending board tasks: pendingBoostUrgency
// plus min(pending, pendingBoostMaxCount). The result is always below
// starvationBoostUrgency (1e12), preserving the starvation guarantee.
func pendingBoostUrgencyFor(pending int) float64 {
	if pending < 0 {
		pending = 0
	}
	if pending > pendingBoostMaxCount {
		pending = pendingBoostMaxCount
	}
	return pendingBoostUrgency + float64(pending)
}

// pendingCacheEntry holds the cached pending-task count for one workdir.
// regMtime is the mtime of the board's fixtures.jsonl registry (zero when
// none exists): the registry decides fixture exclusion (ADV-R05), so a
// registry edit must invalidate the cache even when tasks.jsonl is
// untouched.
type pendingCacheEntry struct {
	count     int
	mtime     time.Time
	regMtime  time.Time
	fetchedAt time.Time
}

// PendingTaskCounter is a thread-safe cache that counts pending tasks on a
// project's board. It reads tasks.jsonl (JSONL format, one JSON object per
// line, counting objects whose "status" == "pending") or falls back to
// tasks.md (markdown boards, counting lines starting with "## [ ] ").
// Boards are re-read when the file mtime changes OR the cache entry is older
// than the TTL — stats are cheap, but full reads of 50-200KB boards must be
// bounded.
//
// ADV-R07: for JSONL boards the raw count is then freshness-checked through
// the R06 git-verified reader (freshnessCheckedPending below) — a pending
// row whose claimed work verifiably landed in git (verdict flip-window or
// flip-overdue) is NOT dispatchable work and stops counting toward the
// pending-boost tier. The check is fail-open: a board or repo the reader
// cannot verify keeps the raw count (never hide work behind a reader
// error). Markdown boards have no evidence fields by construction, so
// their raw count passes through unchanged.
type PendingTaskCounter struct {
	// clk is this component's time seam (SCHED-GAP-169). The zero value
	// reads as the wall clock; NewLoop propagates its own clock here so a
	// test that installs a simulator clock drives the whole component tree,
	// not just evaluate().
	clk clockSeam
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]pendingCacheEntry // keyed by workdir
	// freshnessRead is the R06 reader seam (ADV-R07). A field so tests
	// can stub the git verification; nil means the real
	// ReadBoardFreshness, served through verdictCache.
	freshnessRead func(workdir, boardPath string) FreshnessReport
	// verdictCache is the PERF-002 verdict memo behind the production
	// freshnessRead (cachedFreshnessRead): the git-verified verdict for a
	// board whose file and whose HEAD are both unchanged is by construction
	// the same verdict, so it is not re-derived. Nil (a hand-built counter,
	// or a test that replaced freshnessRead) means no caching.
	verdictCache *FreshnessVerdictCache
}

// cachedFreshnessRead is the production freshness seam: the R06 reader behind
// the PERF-002 verdict cache. A hit costs one os.Stat plus the single HEAD
// probe; a miss runs the full ReadBoardFreshness and stores its verdict.
func (c *PendingTaskCounter) cachedFreshnessRead(workdir, boardPath string) FreshnessReport {
	if c.verdictCache == nil {
		return ReadBoardFreshness(workdir, boardPath, FreshnessOptions{})
	}
	return c.verdictCache.Read(workdir, boardPath)
}

// defaultPendingCounter is the package-level shared instance used by all
// packers that were not given an explicit counter. This keeps existing
// constructors and call sites working unchanged.
var defaultPendingCounter = NewPendingTaskCounter(60 * time.Second)

// NewPendingTaskCounter creates a counter with the given cache TTL for its
// pending COUNT. The PERF-002 verdict cache behind the counter's freshness read
// is separate and longer-lived (DefaultFreshnessVerdictTTL): tying it to the
// count TTL would expire both at the same instant and re-run the git battery on
// every count re-read, which is the cost this cache exists to remove. A
// non-positive count TTL means "cache nothing at all" — the verdict cache is
// disabled too, so a zero-TTL counter behaves exactly as it did before the
// cache (every read fully verified).
func NewPendingTaskCounter(ttl time.Duration) *PendingTaskCounter {
	verdictTTL := DefaultFreshnessVerdictTTL
	if ttl <= 0 {
		verdictTTL = 0
	}
	c := &PendingTaskCounter{
		ttl:          ttl,
		m:            make(map[string]pendingCacheEntry),
		verdictCache: NewFreshnessVerdictCache(verdictTTL, MaxFreshnessVerdictEntries),
	}
	c.freshnessRead = c.cachedFreshnessRead
	return c
}

// CountPending returns the number of pending tasks on the board in the given
// workdir. It reads from the cache when fresh (within TTL and unchanged
// mtime), otherwise re-reads the board file. Malformed lines are skipped
// (never crashes). Returns 0 if no board file exists or the workdir is empty.
func (c *PendingTaskCounter) CountPending(workdir string) int {
	if workdir == "" {
		return 0
	}

	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		// Cache the 0 so we don't stat on every eval for projects without boards.
		c.mu.Lock()
		entry, ok := c.m[workdir]
		if !ok || c.clock().Since(entry.fetchedAt) > c.ttl {
			prev := 0
			if ok {
				prev = entry.count
			}
			c.m[workdir] = pendingCacheEntry{
				count:     0,
				mtime:     time.Time{},
				fetchedAt: c.clock().Now(),
			}
			if prev != 0 {
				log.Printf("PENDING-BOOST: <%s> boost cleared (pending=0)", workdir)
			}
		}
		c.mu.Unlock()
		return 0
	}

	fi, err := os.Stat(boardPath)
	if err != nil {
		return 0
	}

	// ADV-R05: the fixture registry decides exclusion, so its mtime is part
	// of the cache key — an edit to fixtures.jsonl must invalidate even when
	// the board file itself is untouched.
	regMtime := statMtime(fixtureRegistryPath(boardPath))

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.m[workdir]
	cacheFresh := ok && c.clock().Since(entry.fetchedAt) <= c.ttl &&
		entry.mtime.Equal(fi.ModTime()) && entry.regMtime.Equal(regMtime)

	if cacheFresh {
		return entry.count
	}

	prevCount := 0
	if ok {
		prevCount = entry.count
	}

	raw := countPendingBoard(boardPath, fi)
	// ADV-R07: freshness-check the raw count through the R06 reader
	// before it feeds the pending-boost tier — ordering only; cooldown
	// admission is untouched (see the G1/G7 law in board_wake.go).
	count := c.freshnessCheckedPending(workdir, boardPath, raw)
	c.m[workdir] = pendingCacheEntry{
		count:     count,
		mtime:     fi.ModTime(),
		regMtime:  regMtime,
		fetchedAt: c.clock().Now(),
	}

	if count != prevCount {
		log.Printf("PENDING-BOOST: <%s> boosted (pending=%d)", workdir, count)
	}

	return count
}

// freshnessCheckedPending applies the R06 git-verified freshness read to
// the raw pending count (ADV-R07 integration). A pending row whose
// claimed work verifiably landed in the commit graph (verdict
// flip-window or flip-overdue — the implementation→board flip lag the A4
// catalog measured at median 4.4 min) is no longer dispatchable work and
// is subtracted. Fail-open contract: when the reader cannot verify the
// board (unreadable, empty report, or the seam itself is nil) the RAW
// count is returned — a reader failure never hides work.
//
// The subtraction is keyed on the row's RawStatus=="pending" AND its
// verified verdict: only rows the RAW counter counted (raw pending) can
// be subtracted, and only when git proves their work landed. Rows whose
// pointer does not resolve stay counted (open is open — never hide
// work).
func (c *PendingTaskCounter) freshnessCheckedPending(workdir, boardPath string, raw int) int {
	if raw <= 0 || !strings.HasSuffix(boardPath, ".jsonl") {
		return raw // nothing to subtract, or a markdown board (no evidence fields)
	}
	reader := c.freshnessRead
	if reader == nil {
		return raw // nil seam — fail-open, raw count
	}
	rep := reader(workdir, boardPath)
	if rep.TotalRows == 0 && rep.MalformedLines == 0 {
		return raw // unreadable/empty board — keep the raw count
	}
	sub := 0
	for _, v := range rep.Rows {
		if v.RawStatus != "pending" {
			continue // the raw counter never counted this row
		}
		if v.Status == RowFlipWindow || v.Status == RowFlipOverdue {
			sub++
		}
	}
	if sub == 0 {
		return raw
	}
	log.Printf("PENDING-BOOST: <%s> freshness check subtracted %d mid-flip pending row(s) (raw=%d)", workdir, sub, raw)
	if raw-sub < 0 {
		return 0
	}
	return raw - sub
}

// findBoardFile locates the board file for a workdir. It prefers
// .coding-herms/board/tasks.jsonl (JSONL boards) and falls back to
// .coding-herms/tasks.md (tracked-markdown boards). Returns the path and
// true if found, or "" and false if neither exists.
func findBoardFile(workdir string) (string, bool) {
	jsonlPath := filepath.Join(workdir, ".coding-hermes", "board", "tasks.jsonl")
	if fi, err := os.Stat(jsonlPath); err == nil && !fi.IsDir() {
		return jsonlPath, true
	}
	mdPath := filepath.Join(workdir, ".coding-hermes", "tasks.md")
	if fi, err := os.Stat(mdPath); err == nil && !fi.IsDir() {
		return mdPath, true
	}
	return "", false
}

// statMtime returns a file's mtime, or the zero time if it does not exist
// or cannot be stat'ed. Used for the cache-key mtimes where absence is an
// ordinary condition (no registry file), not an error.
func statMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// countPendingBoard reads a board file and returns the count of pending
// tasks. For JSONL files (.jsonl extension) it parses each line as a JSON
// object and counts those whose "status" field == "pending". For markdown
// files it counts lines starting with "## [ ] " (unchecked task headers).
// Malformed lines are silently skipped.
//
// ADV-R05: fixture exclusion is decided BY DATA — the ids declared active
// in the fixtures.jsonl registry beside the board file (plus the
// SCHED-GAP-106 fallback layers: "perpetual":true row flag, NEVER-DONE id
// family) — never by a status-vocabulary accident. See fixture_registry.go.
// The count is intentionally ONLY status=="pending": the pending-vs-open
// rule (see the package comment in board_awareness.go) is that CountPending
// measures dispatchable work, while boardOpenRows (adaptive_cooldown.go)
// is the open-work signal over the wider vocabulary (todo, in_progress, ...).
func countPendingBoard(path string, fi os.FileInfo) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(path)

	count := 0

	if strings.HasSuffix(path, ".jsonl") {
		scanner := bufio.NewScanner(f)
		// Allow lines up to 1MB — board entries can carry large detail blobs.
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				continue // malformed line — skip
			}
			if boardRowIsFixture(obj, fixtureIDs) {
				continue // declared/perpetual fixture — never pending work
			}
			statusRaw, ok := obj["status"]
			if !ok {
				continue
			}
			var status string
			if err := json.Unmarshal(statusRaw, &status); err != nil {
				continue
			}
			if status == "pending" {
				count++
			}
		}
	} else {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) &&
				!registryDeclares(markdownTaskID(line), fixtureIDs) {
				count++
			}
		}
	}

	return count
}

// SetClock installs the clock this PendingTaskCounter reads and waits on (SCHED-GAP-169).
// nil keeps the wall clock. The verdict cache reads the same seam so its TTL
// is driven by the same simulator as the counter's own cache.
func (c *PendingTaskCounter) SetClock(clk clock.Clock) {
	c.clk.Set(clk)
	if c.verdictCache != nil {
		c.verdictCache.SetClock(clk)
	}
}

// clock returns the component's clock, never nil.
func (c *PendingTaskCounter) clock() clock.Clock { return c.clk.Get() }
