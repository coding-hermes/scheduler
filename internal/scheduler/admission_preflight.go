package scheduler

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// tasksAdmissionCounts is the single board-read verdict used by tasks-mode
// admission. Actionable is conservative: it includes rows whose shape the
// classifier does not positively recognise, including rows without criteria.
type tasksAdmissionCounts struct {
	Actionable int
	Blocked    int
	NoCriteria int
	OwnerGated int
	InFlight   int
	Rows       int
}

var (
	cheapSkipCount atomic.Int64
	cheapSkipMu    sync.Mutex
	cheapSkipSeen  = map[string]string{}
)

// CheapAdmissionSkipCount returns the process-lifetime count of cheap skips.
func CheapAdmissionSkipCount() int64 { return cheapSkipCount.Load() }

func admissionBoardFile(workdir string) (string, bool) {
	if workdir == "" {
		return "", false
	}
	path := workdir + "/.coding-hermes/board/tasks.jsonl"
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

func admissionBoardString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func admissionBoardDeferred(obj map[string]json.RawMessage) bool {
	var deferred bool
	return json.Unmarshal(obj["deferred"], &deferred) == nil && deferred
}

// admissionBoardFixtureRow mirrors boardOpenRows' fixture exclusion so the
// cheap-skip classifier and the cooldown metric agree on what "work" means.
func admissionBoardFixtureRow(obj map[string]json.RawMessage) bool {
	var perpetual bool
	_ = json.Unmarshal(obj["perpetual"], &perpetual)
	id := strings.TrimSpace(admissionBoardString(obj["id"]))
	return isFixtureRow(id, perpetual)
}

// tasksAdmissionPreflight reads the same tasks board before a tick is created.
// It is deliberately read-only and fail-open: an unreadable board returns ok=false
// so an evidence failure cannot silently starve a lane.
func tasksAdmissionPreflight(workdir string) (tasksAdmissionCounts, bool) {
	var counts tasksAdmissionCounts
	path, ok := admissionBoardFile(workdir)
	if !ok || !strings.HasSuffix(path, ".jsonl") {
		return counts, ok
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return counts, false
	}
	var objs []map[string]json.RawMessage
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			// A parse error is an evidence gap, not proof that the board is
			// drained. Keep admission fail-open by treating the line as
			// actionable.
			counts.Actionable++
			continue
		}
		objs = append(objs, obj)
	}
	terminal := make(map[string]bool)
	for _, obj := range objs {
		if admissionBoardDeferred(obj) {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(admissionBoardString(obj["id"])))
		if id != "" {
			switch strings.ToLower(strings.TrimSpace(admissionBoardString(obj["status"]))) {
			case "complete", "completed", "done", "closed":
				terminal[id] = true
			}
		}
	}
	for _, obj := range objs {
		status := strings.ToLower(strings.TrimSpace(admissionBoardString(obj["status"])))
		if admissionBoardFixtureRow(obj) {
			// Same contract as boardOpenRows: a registry-declared or
			// perpetual:true fixture is a standing sentinel, never
			// dispatchable work, so it can neither make a lane busy nor
			// excuse a skip (SCHED-GAP-106, ADV-R05).
			continue
		}
		if terminalBoardStatus(status) {
			continue
		}
		if !knownOpenBoardStatus(status) {
			// Unknown status/shape is not positive proof of a non-actionable
			// row. Admit rather than starving a lane on a reader mismatch.
			counts.Actionable++
			continue
		}
		if status == "blocked" {
			if ownerGatedBoardRow(obj) {
				counts.OwnerGated++
			} else {
				counts.Actionable++
			}
			continue
		}
		if ownerGatedBoardRow(obj) {
			counts.OwnerGated++
			continue
		}
		if status == "in_progress" || status == "in-progress" || status == "claimed" || siblingClaimBoardRow(obj) {
			counts.InFlight++
			continue
		}
		counts.Rows++
		depsMet, depsKnown := admissionDepsMet(obj["depends_on"], terminal)
		if !depsKnown {
			counts.Actionable++
			continue
		}
		if !depsMet {
			counts.Blocked++
			continue
		}
		if !usableAcceptanceCriteria(obj["acceptance_criteria"]) {
			counts.NoCriteria++
			// Missing or unreadable criteria are explicitly actionable for
			// this conservative gate. The foreman remains the authority for
			// deciding whether the row can actually be dispatched.
			counts.Actionable++
			continue
		}
		counts.Actionable++
	}
	return counts, true
}

func terminalBoardStatus(status string) bool {
	switch status {
	case "complete", "completed", "done", "closed":
		return true
	default:
		return false
	}
}

func knownOpenBoardStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "in-progress", "claimed", "blocked":
		return true
	default:
		return false
	}
}

func admissionDepsMet(raw json.RawMessage, terminal map[string]bool) (bool, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return true, true
	}
	var deps []string
	if json.Unmarshal(raw, &deps) != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return false, false
		}
		deps = []string{one}
	}
	for _, dep := range deps {
		dep = strings.ToLower(strings.TrimSpace(dep))
		if dep != "" && !terminal[dep] {
			return false, true
		}
	}
	return true, true
}

func ownerGatedBoardRow(obj map[string]json.RawMessage) bool {
	for _, key := range []string{"owner_gate", "owner_gated", "blocked_reason", "blocked_on"} {
		raw := obj[key]
		if strings.TrimSpace(admissionBoardString(raw)) != "" {
			return true
		}
		var gated bool
		if json.Unmarshal(raw, &gated) == nil && gated {
			return true
		}
	}
	return false
}

func siblingClaimBoardRow(obj map[string]json.RawMessage) bool {
	switch strings.ToLower(strings.TrimSpace(admissionBoardString(obj["worker_status"]))) {
	case "claimed", "in_progress", "in-progress":
		return true
	default:
		return false
	}
}

func usableAcceptanceCriteria(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if strings.TrimSpace(string(item)) == "" || string(item) == "null" || string(item) == `""` {
			return false
		}
	}
	return true
}

func noteTasksCheapSkip(project, workdir string, c tasksAdmissionCounts) {
	key := project + "\x00" + workdir
	sig := fmt.Sprintf("%d/%d/%d/%d/%d", c.Actionable, c.Blocked, c.NoCriteria, c.OwnerGated, c.InFlight)
	cheapSkipMu.Lock()
	if cheapSkipSeen[key] == sig {
		cheapSkipMu.Unlock()
		return
	}
	cheapSkipSeen[key] = sig
	cheapSkipMu.Unlock()
	cheapSkipCount.Add(1)
	log.Printf("ADMISSION-CHEAP-SKIP: project=%s reason=%s blocked=%d no_criteria=%d owner_gated=%d in_flight=%d actionable=%d",
		project, "tasks_no_actionable", c.Blocked, c.NoCriteria, c.OwnerGated, c.InFlight, c.Actionable)
}
