// Package blocks provides deploy "blocks" for the coding-hermes scheduler:
// reusable GROUPS (a named list of fleet projects) and TEMPLATES (a named
// list of task definitions) stored as two JSONL files next to the scheduler
// database, plus the machinery to deploy a template to a group — appending
// one pending task row per template task to each member project's foreman
// board (.coding-hermes/board/tasks.jsonl).
//
// Storage mandate (Bane 2026-09): JSONL files, not the SQLite DB. These are
// config-like data that do not need speed; JSONL is easy to back up and for
// other teams to point at git. Each line is one JSON object. Defaults:
//
//	<db dir>/groups.jsonl    — one Group object per line
//	<db dir>/templates.jsonl — one Template object per line
//
// The two files are path-overridable via the --groups-file / --templates-file
// CLI flags and the [scheduler] TOML keys (groups_file / templates_file) —
// see cmd/schedulerd/main.go for the resolution order.
//
// Concurrency model: the JSONL stores are config-like and small, so every
// mutation is a locked read-modify-write through a temp file + atomic rename
// (never an in-place append to the live file). Readers parse the file each
// call, so they always observe either the old or the new inode — never a
// partial write. Board appends (deploy) go the other way on purpose: foreman
// tasks.jsonl files are append-only streams, so deploy appends with O_APPEND.
package blocks

import (
	"errors"
	"fmt"
	"strings"
)

// Errors returned by Store and deploy operations. API handlers map these to
// HTTP status codes (409 for ErrExists, 404 for ErrNotFound).
var (
	// ErrNotFound is returned when a named group or template does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when a group or template with the name already exists.
	ErrExists = errors.New("already exists")
	// ErrNoBoard is returned when a project workdir has no JSONL task board
	// (.coding-hermes/board/tasks.jsonl) to append rows to.
	ErrNoBoard = errors.New("no JSONL board found")
	// ErrWorkdirMissing is returned when a project workdir does not exist on
	// this host (or its path is empty).
	ErrWorkdirMissing = errors.New("workdir missing")
)

// Group is a named, ordered list of scheduler projects that a template can
// be deployed to in a single operation. Projects are referenced by scheduler
// project NAME (the projects table primary key).
type Group struct {
	Name        string   `json:"name"`
	Projects    []string `json:"projects"`
	Description string   `json:"description"`
}

// GroupUpdate is a partial group update — only non-nil fields are applied
// (same pointer semantics as database.NamespacePatch). The name is immutable
// and always comes from the URL path.
type GroupUpdate struct {
	Projects    *[]string `json:"projects,omitempty"`
	Description *string   `json:"description,omitempty"`
}

// TemplateTask is a single task definition inside a Template.
//
// IDPattern is optional and defaults to DefaultTaskIDPattern. Recognized
// placeholders (substituted at deploy time):
//
//	{TEMPLATE} — template name
//	{DATE}     — UTC deploy date, YYYYMMDD (e.g. 20260903)
//	{PROJECT}  — target project name
//	{TASK}     — 1-based task ordinal within the template, zero-padded (01..)
//
// An IDPattern that does not carry {TASK} (or {N}) gets "-{TASK}" appended
// automatically so a multi-task template can never produce colliding ids.
// Title and Detail also get {TEMPLATE}/{DATE}/{PROJECT} substitution (a
// per-project title like "Audit {PROJECT} board hygiene" is the point of
// template blocks). Labels become the row's capability_tags.
type TemplateTask struct {
	IDPattern string   `json:"id_pattern,omitempty"`
	Title     string   `json:"title"`
	Detail    string   `json:"detail,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// Template is a named list of task definitions that can be deployed to every
// project in a group. Each deploy appends one pending board row per task to
// each member project's board (idempotent per template/date/project).
type Template struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Tasks       []TemplateTask `json:"tasks"`
}

// TemplateUpdate is a partial template update — only non-nil fields applied.
type TemplateUpdate struct {
	Description *string         `json:"description,omitempty"`
	Tasks       *[]TemplateTask `json:"tasks,omitempty"`
}

// DefaultTaskIDPattern is the task id pattern used when neither the template
// nor the task defines one. The {TASK} ordinal keeps ids unique within a
// multi-task template; the idempotency prefix is everything before it.
const DefaultTaskIDPattern = "{TEMPLATE}-{DATE}-{PROJECT}-{TASK}"

// ValidateGroup checks a group's required fields. Names must be non-empty
// and free of whitespace so they stay URL- and id-safe.
func ValidateGroup(g Group) error {
	if strings.TrimSpace(g.Name) == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(g.Name, " \t\n") {
		return fmt.Errorf("name %q must not contain whitespace", g.Name)
	}
	return nil
}

// ValidateTemplate checks a template's required fields: non-empty whitespace
// -free name and at least one task, each with a non-empty title.
func ValidateTemplate(t Template) error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("name is required")
	}
	if strings.ContainsAny(t.Name, " \t\n") {
		return fmt.Errorf("name %q must not contain whitespace", t.Name)
	}
	if len(t.Tasks) == 0 {
		return errors.New("tasks are required — a template with no tasks deploys nothing")
	}
	for i, task := range t.Tasks {
		if strings.TrimSpace(task.Title) == "" {
			return fmt.Errorf("tasks[%d].title is required", i)
		}
	}
	return nil
}
