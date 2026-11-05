package scheduler

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// SCHED-GAP-NODRAIN: a WORKABLE-row scanner for tasks-mode admission.
//
// The defect (verified by the foreman at HEAD 22906c2c, 2026-11-05):
// tasksAdmissionDue keyed its waiver on boardOpenRows, the shared OPEN-row
// counter. That counter answers "is any row open?", not "is any row
// DISPATCHABLE?": it counts every open-vocabulary row regardless of its
// depends_on, and its vocabulary has no notion of blocked. A tasks-mode
// lane whose board held only blocked rows, or only rows whose dependencies
// are unmet, was therefore never "drained" — the waiver re-admitted it on
// the 5s eval debounce forever, and every spawned tick burned a slot and
// an LLM session discovering there was nothing to dispatch (the
// SCHED-GAP-1655/1656 no-op-tick waste class, one level deeper: the rows
// were OPEN, just not WORKABLE).
//
// "Drained" must mean "no WORKABLE row". A row is workable when:
//   - its status is open by the EXISTING fleet vocabulary (openStatuses,
//     board_freshness.go — the same list boardOpenRows counts; no second
//     status list exists after this row), AND
//   - its status is not blocked (blockedStatusLiteral, case-insensitive).
//     Belt-and-braces today: "blocked" is not in the open vocabulary, so
//     an open-vocabulary row can never BE blocked — but the rule must stay
//     true by construction if the open vocabulary ever widens to include
//     blocked spellings, AND
//   - every id in its depends_on is RESOLVED: the referenced row exists on
//     the board AND its status is terminal (completeStatuses:
//     complete/completed/done/closed). An absent dependency row, and a
//     dependency still open or blocked, are UNMET — deliberately the
//     simplest defensible rule; block-propagation chains ("non-terminal
//     and not itself blocked-only") were considered and rejected as a
//     second transitive-closure semantics the board does not carry.
//
// Everything boardOpenRows excludes, this scanner excludes through the
// SAME helpers on the SAME parse: the row struct is the open scanner's own
// (id/status/perpetual/deferred, strict types — a row the open scanner
// cannot parse is malformed here too), the fixture layers are
// isFixtureRow (perpetual flag + NEVER-DONE family, SCHED-GAP-106) and
// registryDeclares (ADV-R05), and deferred rows are excluded identically
// (SCHED-GAP-1727). A MALFORMED line still counts workable — the doctrine
// that a parse error must never hide work; boardOpenRows counts the same
// line open. Markdown boards mirror the open scanner's branch: an
// unchecked, non-fixture header is workable (markdown carries no
// depends_on, so there is nothing further to resolve).
//
// Why a NEW function instead of narrowing boardOpenRows: boardOpenRows is
// the shared open-work signal — the adaptive-cooldown escalation branch
// (adaptive_cooldown.go), the two builder no-work gates and the deferral
// logger in nowork.go all read it, and its counts are load-bearing for
// pacing decisions this row must not perturb. Workability is ADMISSION
// policy for tasks mode, so it gets its own scanner and leaves the shared
// counter byte-for-byte unchanged (pinned by
// TestSCHEDGAPNODRAIN_OpenRowsUnchanged).
//
// Dependency details:
//   - Forward references resolve: the id → terminal index is built over
//     the whole file before any row is classified, so a row may depend on
//     a row appended after it. Later occurrences of an id win (the JSONL
//     convention: append supersedes).
//   - Id matching is trim + case-insensitive: board ids are hand-written
//     and case drift between a depends_on entry and its row is common.
//   - Shape tolerance: absent field, null, [] and ["A", ...] are the real
//     writer shapes (null is the deployed-row default — unknown fields are
//     null per the 31-key board schema; JSON null unmarshals into the
//     slice as nil, no error). A single string is accepted as a
//     one-element list. Any other shape (number, object, mixed array)
//     cannot be read as dependencies and is treated UNMET: a dependency
//     field this scanner cannot read may hold an unmet dependency, and
//     refusing admission on it only costs the lane its cooldown pin — the
//     opposite trade (admit) is the defect this row closes. An empty
//     string inside the list is writer noise, not a gate.
//
// Return contract mirrors boardOpenRows: ok is false when the board is
// missing or unreadable (fail-open unchanged — callers treat the
// observation as absent, never as zero), and also when the scan errors
// mid-file: the observation must be complete to be acted on.
func boardWorkableRows(workdir string) (int, bool) {
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return 0, false
	}
	f, err := os.Open(boardPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	fixtureIDs := loadFixtureRegistry(boardPath)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	isJSONL := strings.HasSuffix(boardPath, ".jsonl")

	// Pass 1: parse every JSONL line once (the open scanner's struct, plus
	// the raw depends_on), build the id → terminal index, and accumulate
	// the rows that are workable by construction (malformed lines, and
	// markdown headers on legacy boards).
	//
	// parsedRow keeps the fields pass 2 needs; status is stored exactly the
	// way boardOpenRows reads it (strings.ToLower, no trim) so the two
	// scanners cannot drift on vocabulary membership.
	type parsedRow struct {
		deps     json.RawMessage
		status   string // lowercased, no trim — boardOpenRows parity
		fixture  bool   // registry / perpetual / NEVER-DONE (SCHED-GAP-106, ADV-R05)
		deferred bool   // SCHED-GAP-1727
	}
	var rows []parsedRow
	terminal := map[string]bool{} // id (lowercased) → its status is terminal
	workable := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !isJSONL {
			// Markdown boards: unchecked headers are open work (fixture
			// rows excluded identically to boardOpenRows); markdown has
			// no dependencies, so openness is workability.
			if strings.HasPrefix(line, "## [ ] ") && !isFixtureLine(line) &&
				!registryDeclares(markdownTaskID(line), fixtureIDs) {
				workable++
			}
			continue
		}
		var row struct {
			ID        string          `json:"id"`
			Status    string          `json:"status"`
			Perpetual bool            `json:"perpetual"`
			Deferred  bool            `json:"deferred"`
			DependsOn json.RawMessage `json:"depends_on"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			workable++ // malformed row — count workable, never hide work
			continue
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id != "" {
			terminal[id] = completeStatuses[strings.ToLower(row.Status)]
		}
		rows = append(rows, parsedRow{
			deps:     row.DependsOn,
			status:   strings.ToLower(row.Status),
			fixture:  isFixtureRow(row.ID, row.Perpetual) || registryDeclares(row.ID, fixtureIDs),
			deferred: row.Deferred,
		})
	}
	if err := scanner.Err(); err != nil {
		return 0, false
	}

	// Pass 2: classify the parsed rows. Every exclusion matches
	// boardOpenRows branch-for-branch; the two additions are the blocked
	// check and the depends_on resolution.
	for _, r := range rows {
		if r.fixture {
			continue // declared/perpetual fixture — not work (SCHED-GAP-106, ADV-R05)
		}
		if r.deferred {
			continue // deferred:true — visible but not actionable (SCHED-GAP-1727)
		}
		if !openStatuses[r.status] {
			continue // not open by the existing vocabulary — not work
		}
		if r.status == blockedStatusLiteral {
			continue // belt-and-braces: blocked is never workable
		}
		if !boardRowDepsMet(r.deps, terminal) {
			continue // unmet depends_on — not dispatchable yet
		}
		workable++
	}
	return workable, true
}

// boardRowDepsMet reports whether every depends_on id of a parsed board
// row resolves to a TERMINAL row in terminal (id → status-is-terminal).
// An absent, null or empty depends_on is met; a dependency that is absent
// from the board, or still open/blocked, is unmet; an unreadable
// dependency shape is unmet (see boardWorkableRows for the tolerance
// table and the rationale).
func boardRowDepsMet(deps json.RawMessage, terminal map[string]bool) bool {
	if len(deps) == 0 {
		return true // field absent — nothing to wait on
	}
	var ids []string
	if err := json.Unmarshal(deps, &ids); err != nil {
		// JSON null unmarshals into the slice cleanly above (nil, no
		// error), so reaching here means a non-null, non-string-array
		// shape. Accept a single string as a one-element list; anything
		// else cannot be read as dependencies → unmet.
		var single string
		if err := json.Unmarshal(deps, &single); err != nil {
			return false
		}
		ids = []string{single}
	}
	for _, d := range ids {
		id := strings.ToLower(strings.TrimSpace(d))
		if id == "" {
			continue // empty dep id — writer noise, not a gate
		}
		if !terminal[id] {
			return false // absent, open, or blocked dependency — unmet
		}
	}
	return true
}
