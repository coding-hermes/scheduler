package scheduler

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ADV-R05 (G10): the canonical fixture registry.
//
// A board's DECLARED fixtures live in fixtures.jsonl, the sibling of the
// board file itself (.coding-hermes/board/fixtures.jsonl next to
// tasks.jsonl; fixtures.jsonl beside a legacy tasks.md). Each line is
// {"id": "...", "active": true|false, ...}. A row whose id is declared
// ACTIVE is excluded from board-work counting BY DATA — no matter what
// status the row carries, so a fixture can never become "real work" by a
// status-vocabulary accident (pre-ADV-R05, E2E-001 and GITREINS-JUDGE were
// invisible to CountPending only while their status stayed todo/complete).
// "active":false retires the exclusion: the row counts as work again.
// A missing "active" field defaults to active (the fleet's existing
// fixtures.jsonl files omit it).
//
// The registry is the authoritative layer; two fallback layers keep older
// boards working untouched (fleet sweep 2026-09-15, 56 boards: 34 rows
// carry "perpetual":true, 6 NEVER-DONE-family rows carry no flag, and one
// board declares nothing at all):
//
//  1. the row flag  — "perpetual": true on the task row (SCHED-GAP-106)
//  2. the id shape  — ids in the NEVER-DONE family (prefix, any suffix,
//                    case-insensitive), the fleet-standard fixture id
//
// Every layer excludes in BOTH consumers — CountPending (board_awareness.go,
// the pending-work boost) and boardOpenRows (adaptive_cooldown.go, the
// open-row baseline) — so a fixture can neither boost urgency nor hold a
// finished project's adaptive cooldown fast.

// fixtureRegistryEntry is one parsed fixtures.jsonl line.
type fixtureRegistryEntry struct {
	ID     string `json:"id"`
	Active *bool  `json:"active"` // nil (absent) means active
}

// fixtureRegistryPath returns the canonical registry location for a board
// file: <workdir>/.coding-hermes/board/fixtures.jsonl. For JSONL boards the
// board file itself lives in that board/ directory, so the registry is its
// sibling; for legacy markdown boards (tasks.md directly in .coding-hermes/)
// the registry still lives in the sibling board/ directory. Fleet sweep
// 2026-09-15: all 51 existing registries — including every markdown-board
// repo — sit at board/fixtures.jsonl and none anywhere else.
func fixtureRegistryPath(boardPath string) string {
	dir := filepath.Dir(boardPath)
	if filepath.Base(dir) == "board" {
		return filepath.Join(dir, "fixtures.jsonl")
	}
	return filepath.Join(dir, "board", "fixtures.jsonl")
}

// loadFixtureRegistry reads the registry beside a board file and returns
// the set of ACTIVE fixture ids, lowercased for exact-match comparison.
// A missing file, an unreadable file, and malformed lines are all tolerated
// (nil/empty result): a broken registry must never crash board counting,
// and the fallback exclusion layers still apply.
func loadFixtureRegistry(boardPath string) map[string]bool {
	f, err := os.Open(fixtureRegistryPath(boardPath))
	if err != nil {
		return nil
	}
	defer f.Close()

	var ids map[string]bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var entry fixtureRegistryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // malformed registry line — skip
		}
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" {
			continue
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		if entry.Active == nil || *entry.Active {
			ids[id] = true // active (default) — declared fixture
		} else {
			delete(ids, id) // explicitly retired — not a fixture
		}
	}
	return ids
}

// registryDeclares reports whether id is declared an active fixture in the
// registry. Exact id match only — lookalikes (E2E-0011, RE-RUN-E2E-001,
// NEVER-DONELESS) never match, so exclusion by data cannot recreate the
// old substring/prefix accidents in the other direction.
func registryDeclares(id string, fixtureIDs map[string]bool) bool {
	if len(fixtureIDs) == 0 {
		return false
	}
	return fixtureIDs[strings.ToLower(strings.TrimSpace(id))]
}

// boardRowIsFixture reports whether a parsed JSONL board row is a fixture,
// applying the canonical layer first and the fallback layers second:
//
//  1. registry declaration — the row's id is an active fixtures.jsonl entry
//     (ADV-R05; excludes regardless of status or row flags)
//  2. "perpetual": true on the row (SCHED-GAP-106)
//  3. NEVER-DONE id family (SCHED-GAP-106 fallback for boards that carry
//     the fleet-standard fixture without declaring or flagging it)
//
// Used by both board-work consumers — countPendingBoard (board_awareness.go)
// and boardOpenRows (adaptive_cooldown.go) — so all layers exclude in both.
func boardRowIsFixture(obj map[string]json.RawMessage, fixtureIDs map[string]bool) bool {
	if perpRaw, ok := obj["perpetual"]; ok {
		var perp bool
		if json.Unmarshal(perpRaw, &perp) == nil && perp {
			return true
		}
	}
	if idRaw, ok := obj["id"]; ok {
		var id string
		if json.Unmarshal(idRaw, &id) == nil {
			if registryDeclares(id, fixtureIDs) {
				return true
			}
			if isFixtureRow(id, false) {
				return true
			}
		}
	}
	return false
}

// markdownTaskID extracts the task id (first token) from a markdown board
// header line of the form "## [ ] ID - title" or "## [x] ID: title".
// Trailing id punctuation (":", "-", ",") is stripped.
func markdownTaskID(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "##"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "[ ]"))
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "[x]"))
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimRight(rest, ":-,")
}
