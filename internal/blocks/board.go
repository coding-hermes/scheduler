package blocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Board task-row schema (canonical 31-key foreman board shape — see
// .coding-hermes/board/schema.sql in any JSONL-canonical fleet repo).
// Unknown fields are null (nulls are valid; missing keys are not — board
// readers and duckdb caches assume one uniform key set). Field order mirrors
// the canonical rows so diffs against hand-written boards stay minimal.

// BoardTaskRow is one pending task row appended to a foreman board's
// tasks.jsonl by a template deploy.
type BoardTaskRow struct {
	ID               string      `json:"id"`
	Title            string      `json:"title"`
	Status           string      `json:"status"`
	Priority         string      `json:"priority"`
	Complexity       int         `json:"complexity"`
	DependsOn        interface{} `json:"depends_on"`
	Blocks           interface{} `json:"blocks"`
	PrimaryModel     interface{} `json:"primary_model"`
	PrimaryProvider  interface{} `json:"primary_provider"`
	FallbackModel    interface{} `json:"fallback_model"`
	FallbackProvider interface{} `json:"fallback_provider"`
	Reasoning        interface{} `json:"reasoning"`
	CapabilityTags   []string    `json:"capability_tags"`
	WorkerStatus     string      `json:"worker_status"`
	DispatchedAt     interface{} `json:"dispatched_at"`
	CompletedAt      interface{} `json:"completed_at"`
	Attempts         int         `json:"attempts"`
	ExitCode         interface{} `json:"exit_code"`
	CommitHash       interface{} `json:"commit_hash"`
	FilesChanged     interface{} `json:"files_changed"`
	LinesAdded       interface{} `json:"lines_added"`
	LinesRemoved     interface{} `json:"lines_removed"`
	GuardResult      interface{} `json:"guard_result"`
	CIResult         interface{} `json:"ci_result"`
	WorkerSummary    interface{} `json:"worker_summary"`
	ForemanNote      string      `json:"foreman_note"`
	BlockedReason    interface{} `json:"blocked_reason"`
	ReviewNotes      interface{} `json:"review_notes"`
	CreatedAt        string      `json:"created_at"`
	UpdatedAt        string      `json:"updated_at"`
	BlockedSince     interface{} `json:"blocked_since"`
}

// boardTimeFormat matches the canonical board timestamp convention
// (space-separated UTC with microsecond precision — see the full-schema
// task-row injection reference).
const boardTimeFormat = "2006-01-02 15:04:05.000000"

// DefaultPriority is stamped on deployed rows when the template task does
// not express one (the template schema has no priority field; foremen
// re-triage as usual).
const DefaultPriority = "P2"

// boardStatusPending is the open-row status every deployed row starts in.
const boardStatusPending = "pending"

// closedBoardStatus is the status that makes a row closed for idempotency.
const closedBoardStatus = "complete"

// substituteTaskPattern expands {TEMPLATE}, {DATE} and {PROJECT} in a task
// id pattern. {TASK}/{N} handling happens in taskID (it needs the ordinal).
func substituteTaskPattern(pattern, templateName, project, date string) string {
	r := strings.NewReplacer(
		"{TEMPLATE}", templateName,
		"{DATE}", date,
		"{PROJECT}", project,
	)
	return r.Replace(pattern)
}

// expandTitle substitutes the shared placeholders in a task title/detail so
// a template can carry per-project wording.
func expandTitle(s, templateName, project, date string) string {
	return substituteTaskPattern(s, templateName, project, date)
}

// taskID computes a task row's concrete id and its idempotency prefix for one
// (template, project, date) deploy. prefix is the deployment signature —
// everything the {TASK} ordinal varies. An IDPattern without a task ordinal
// gets "-{TASK}" appended so multi-task templates never collide.
func taskID(pattern, templateName, project, date string, idx int) (id, prefix string) {
	p := substituteTaskPattern(pattern, templateName, project, date)
	ordinal := fmt.Sprintf("%02d", idx)
	if strings.Contains(p, "{TASK}") || strings.Contains(p, "{N}") {
		prefix = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(p, "{TASK}", ""), "{N}", ""), "-")
		id = strings.ReplaceAll(strings.ReplaceAll(p, "{TASK}", ordinal), "{N}", ordinal)
		return id, prefix
	}
	prefix = p
	id = p + "-" + ordinal
	return id, prefix
}

// BuildTaskRows renders a template into concrete pending board rows for one
// project. date is the YYYYMMDD UTC deploy date (taskID's {DATE} token).
// foremanNote carries the deploy provenance line stored on every row.
func BuildTaskRows(t Template, project, date, foremanNote string) []BoardTaskRow {
	now := time.Now().UTC().Format(boardTimeFormat)
	rows := make([]BoardTaskRow, 0, len(t.Tasks))
	for i, task := range t.Tasks {
		pattern := task.IDPattern
		if pattern == "" {
			pattern = DefaultTaskIDPattern
		}
		id, _ := taskID(pattern, t.Name, project, date, i+1)
		row := BoardTaskRow{
			ID:             id,
			Title:          expandTitle(task.Title, t.Name, project, date),
			Status:         boardStatusPending,
			Priority:       DefaultPriority,
			Complexity:     2,
			CapabilityTags: task.Labels,
			WorkerStatus:   boardStatusPending,
			ForemanNote:    foremanNote,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if len(task.Labels) == 0 {
			row.CapabilityTags = []string{}
		}
		if detail := expandTitle(task.Detail, t.Name, project, date); detail != "" {
			// Task rows have no dedicated detail column; the fleet
			// convention for injected rows is to carry the spec in
			// reasoning.note (JSON), keeping the title short.
			row.Reasoning = map[string]interface{}{"note": detail}
		}
		rows = append(rows, row)
	}
	return rows
}

// boardTasksPath returns the canonical JSONL task board for a workdir.
func boardTasksPath(workdir string) string {
	return filepath.Join(workdir, ".coding-hermes", "board", "tasks.jsonl")
}

// boardRow is the minimal view of an existing board row needed by the
// idempotency scan.
type boardRow struct {
	ID     string
	Status string
}

// scanBoardRows parses an existing tasks.jsonl tolerantly (malformed lines
// and a torn final line are skipped — a broken board must never abort a
// deploy batch) and returns every row's id + status.
func scanBoardRows(path string) ([]boardRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []boardRow
	if len(data) == 0 {
		return rows, nil
	}
	hasFinalNL := data[len(data)-1] == '\n'
	lines := bytes.Split(data, []byte{'\n'})
	if !hasFinalNL {
		lines = lines[:len(lines)-1]
		warnf("torn final line in board %s — skipped during deploy idempotency scan", path)
	}
	for _, raw := range lines {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var obj struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(line, &obj); err != nil {
			warnf("skipping malformed board line in %s: %v", path, err)
			continue
		}
		rows = append(rows, boardRow{ID: obj.ID, Status: obj.Status})
	}
	return rows, nil
}

// boardStatusOpen reports whether a row is open (not closed). A row is
// closed only when status == "complete"; pending/in_progress/blocked rows
// are all open.
func boardStatusOpen(status string) bool {
	return status != closedBoardStatus
}

// AppendTasksResult describes one project's outcome inside a deploy.
type AppendTasksResult struct {
	// Appended lists the concrete task ids written (nil when skipped or errored).
	Appended []string
	// Skipped is true when the board already carries this deployment
	// (idempotent skip — see planAppend).
	Skipped bool
	// SkipReason explains why the project was skipped.
	SkipReason string
}

// planAppend is the READ-ONLY half of a board append: it resolves the board
// path from the workdir, scans existing rows tolerantly, and decides whether
// the deployment is new (returns clean), already present (skipped=true +
// reason), or impossible (typed error: ErrWorkdirMissing / ErrNoBoard / io).
// Dry runs call this and stop; live appends call it and then write. A broken
// board (malformed lines, torn tail) never aborts the plan — bad lines are
// skipped with a logged warning, matching the JSONL-tolerance doctrine.
func planAppend(workdir string, rows []BoardTaskRow) (skipped bool, skipReason string, err error) {
	if workdir == "" {
		return false, "", fmt.Errorf("workdir is empty: %w", ErrWorkdirMissing)
	}
	if fi, err := os.Stat(workdir); err != nil || !fi.IsDir() {
		return false, "", fmt.Errorf("workdir %q: %w", workdir, ErrWorkdirMissing)
	}
	path := boardTasksPath(workdir)
	if _, err := os.Stat(path); err != nil {
		return false, "", fmt.Errorf("%s: %w", path, ErrNoBoard)
	}
	if len(rows) == 0 {
		return false, "", nil
	}

	existing, err := scanBoardRows(path)
	if err != nil {
		return false, "", fmt.Errorf("read board %s: %w", path, err)
	}
	planned := make(map[string]bool, len(rows))
	for _, r := range rows {
		planned[r.ID] = true
	}
	// Deployment prefix: all planned ids share it (they differ only in the
	// trailing task ordinal). Board rows carrying it identify this deploy.
	prefix := rows[0].ID
	if i := strings.LastIndex(prefix, "-"); i > 0 {
		prefix = prefix[:i]
	}
	for _, ex := range existing {
		switch {
		case planned[ex.ID]:
			return true, fmt.Sprintf("board row %q already exists (same deployment date) — re-deploy is a no-op", ex.ID), nil
		case boardStatusOpen(ex.Status) && (strings.HasPrefix(ex.ID, prefix+"-") || ex.ID == prefix):
			return true, fmt.Sprintf("open board row %q already carries this template's id prefix %q", ex.ID, prefix), nil
		}
	}
	return false, "", nil
}

// AppendTasks appends pending task rows for one deploy to a project's board.
// It resolves the board path from the workdir and returns typed errors that
// the deploy engine turns into per-project outcomes:
//
//   - ErrWorkdirMissing — workdir empty or not present on this host;
//   - ErrNoBoard — workdir exists but has no .coding-hermes/board/tasks.jsonl;
//   - os errors from reading/appending the board file itself.
//
// Idempotency (never double-deploy): the project is SKIPPED when its board
// already carries this deployment signature — either an OPEN row whose id
// starts with the template's id-prefix for this (template, date, project),
// or ANY row (open or closed) whose id exactly equals a planned id (a
// same-day re-deploy after completion would otherwise duplicate ids).
func AppendTasks(workdir string, rows []BoardTaskRow) (AppendTasksResult, error) {
	var res AppendTasksResult
	skipped, reason, err := planAppend(workdir, rows)
	if err != nil {
		return res, err
	}
	if skipped {
		res.Skipped = true
		res.SkipReason = reason
		return res, nil
	}
	if len(rows) == 0 {
		return res, nil
	}
	path := boardTasksPath(workdir)

	// O_RDWR (not just O_WRONLY) so the torn-tail probe below can ReadAt the
	// last byte; appends still go through O_APPEND.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0o644)
	if err != nil {
		return res, fmt.Errorf("open board %s for append: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// If the file ends mid-line (torn tail), separate it from our rows with
	// a leading newline so we never splice a row into the fragment.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		tail := make([]byte, 1)
		if _, err := f.ReadAt(tail, fi.Size()-1); err == nil && tail[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return res, fmt.Errorf("separate torn tail in %s: %w", path, err)
			}
		}
	}
	for _, r := range rows {
		line, err := json.Marshal(r)
		if err != nil {
			return res, fmt.Errorf("marshal row %s: %w", r.ID, err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return res, fmt.Errorf("append row %s to %s: %w", r.ID, path, err)
		}
		res.Appended = append(res.Appended, r.ID)
	}
	if err := f.Sync(); err != nil {
		return res, fmt.Errorf("sync board %s: %w", path, err)
	}
	return res, nil
}
