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
// (c) Fixture rows — .coding-hermes/board/fixtures.jsonl (E2E-001,
//     NEVER-DONE, GITREINS-JUDGE) — are perpetual recurring chores, not
//     dispatchable tasks. The foreman runs them on cadence (E2E-001 light
//     every tick, full battery every +5; NEVER-DONE full 12-point every +3),
//     so their tasks.jsonl rows legitimately stay status=pending/attempts=0
//     forever. Run evidence is in each row's worker_summary and in
//     events.jsonl detail.fixtures (e.g. "E2E-001 FULL battery ran tick #344
//     ... next full due #349+"); pending/attempts=0 on a fixture is EXPECTED.
//
// (d) Blocked rows — status=blocked with blocked_reason set (e.g. FIX-STACK,
//     "Bane defers (systemd enable decision)") — are USER-GATED and stay
//     blocked until an operator unblocks; CountPending below deliberately
//     counts only status=="pending", never blocked rows.
//
// (e) The boost below affects ONLY (a): pending rows raise PROJECT urgency so
//     a project with fresh board work is picked sooner. It cannot dispatch,
//     unblock, or complete a task row. Fixture rows count toward the pending
//     total, so projects with active fixtures carry a permanent (benign)
//     pending boost.

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
type pendingCacheEntry struct {
	count     int
	mtime     time.Time
	fetchedAt time.Time
}

// PendingTaskCounter is a thread-safe cache that counts pending tasks on a
// project's board. It reads tasks.jsonl (JSONL format, one JSON object per
// line, counting objects whose "status" == "pending") or falls back to
// tasks.md (markdown boards, counting lines starting with "## [ ] ").
// Boards are re-read when the file mtime changes OR the cache entry is older
// than the TTL — stats are cheap, but full reads of 50-200KB boards must be
// bounded.
type PendingTaskCounter struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]pendingCacheEntry // keyed by workdir
}

// defaultPendingCounter is the package-level shared instance used by all
// packers that were not given an explicit counter. This keeps existing
// constructors and call sites working unchanged.
var defaultPendingCounter = NewPendingTaskCounter(60 * time.Second)

// NewPendingTaskCounter creates a counter with the given cache TTL.
func NewPendingTaskCounter(ttl time.Duration) *PendingTaskCounter {
	return &PendingTaskCounter{
		ttl: ttl,
		m:   make(map[string]pendingCacheEntry),
	}
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
		if !ok || time.Since(entry.fetchedAt) > c.ttl {
			prev := 0
			if ok {
				prev = entry.count
			}
			c.m[workdir] = pendingCacheEntry{
				count:     0,
				mtime:     time.Time{},
				fetchedAt: time.Now(),
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

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.m[workdir]
	cacheFresh := ok && time.Since(entry.fetchedAt) <= c.ttl && entry.mtime.Equal(fi.ModTime())

	if cacheFresh {
		return entry.count
	}

	prevCount := 0
	if ok {
		prevCount = entry.count
	}

	count := countPendingBoard(boardPath, fi)
	c.m[workdir] = pendingCacheEntry{
		count:     count,
		mtime:     fi.ModTime(),
		fetchedAt: time.Now(),
	}

	if count != prevCount {
		log.Printf("PENDING-BOOST: <%s> boosted (pending=%d)", workdir, count)
	}

	return count
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

// countPendingBoard reads a board file and returns the count of pending
// tasks. For JSONL files (.jsonl extension) it parses each line as a JSON
// object and counts those whose "status" field == "pending". For markdown
// files it counts lines starting with "## [ ] " (unchecked task headers).
// Malformed lines are silently skipped.
func countPendingBoard(path string, fi os.FileInfo) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

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
			// SCHED-GAP-106: perpetual fixtures are never pending work.
			if idRaw, ok := obj["id"]; ok {
				var id string
				if json.Unmarshal(idRaw, &id) == nil && isFixtureRow(id, false) {
					continue
				}
			}
			if perpRaw, ok := obj["perpetual"]; ok {
				var perp bool
				if json.Unmarshal(perpRaw, &perp) == nil && perp {
					continue
				}
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
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) {
				count++
			}
		}
	}

	return count
}
