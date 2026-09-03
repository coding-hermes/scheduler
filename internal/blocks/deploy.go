package blocks

import (
	"fmt"
	"strings"
	"time"
)

// ProjectTarget is one deploy destination, resolved by the API layer from
// the scheduler projects table (name + workdir). A target with an unknown or
// empty workdir becomes a per-project error — never a batch abort.
type ProjectTarget struct {
	Name    string
	Workdir string
}

// DeployRequest is a single template→group deploy operation.
type DeployRequest struct {
	// Group is the resolved group being deployed to.
	Group Group
	// Template is the resolved template whose task rows get appended.
	Template Template
	// Projects are the resolved member targets (deduped by name).
	Projects []ProjectTarget
	// DryRun plans the deploy without writing to any board.
	DryRun bool
	// Now is the deploy timestamp; time.Now() when zero (tests inject a
	// fixed time for deterministic ids).
	Now time.Time
	// ForemanNote is an optional provenance suffix for deployed rows
	// (the API passes group/template context).
	ForemanNote string
}

// ProjectOutcome reports one project's result inside a deploy response.
// Status vocabulary:
//
//	appended     — task rows written (live deploy)
//	would_append — task rows would be written (dry run)
//	skipped      — already deployed (live deploy)
//	would_skip   — would be skipped (dry run)
//	error        — per-project failure (missing workdir / no board / IO);
//	               the rest of the batch still runs
type ProjectOutcome struct {
	Project string   `json:"project"`
	Workdir string   `json:"workdir"`
	Status  string   `json:"status"`
	TaskIDs []string `json:"task_ids,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// DeploySummary aggregates a deploy batch.
type DeploySummary struct {
	Projects int `json:"projects"`
	Appended int `json:"appended"` // appended + would_append (dry runs)
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
	TaskRows int `json:"task_rows"` // total planned task rows (appended+would_append rows)
}

// DeployResult is the full deploy response body.
type DeployResult struct {
	Group      string           `json:"group"`
	Template   string           `json:"template"`
	DryRun     bool             `json:"dry_run"`
	DeployedAt string           `json:"deployed_at"`
	Projects   []ProjectOutcome `json:"projects"`
	Summary    DeploySummary    `json:"summary"`
}

// deployDate renders the {DATE} token: YYYYMMDD in UTC.
func deployDate(now time.Time) string {
	return now.UTC().Format("20060102")
}

// Deploy runs a template→group deploy. Group member names are resolved via
// the already-looked-up Projects targets; a member missing from that slice
// (not in the scheduler projects table) is reported as a per-project error
// and the batch continues. Idempotency is per project (see AppendTasks):
// projects whose board already carries the deployment are skipped, never
// double-written. In DryRun mode no board file is touched — the result lists
// what WOULD happen (would_append / would_skip / error for would-fail
// targets). One event-log entry per deploy is the API layer's job.
func Deploy(req DeployRequest) DeployResult {
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	date := deployDate(now)
	res := DeployResult{
		Group:      req.Group.Name,
		Template:   req.Template.Name,
		DryRun:     req.DryRun,
		DeployedAt: now.UTC().Format(time.RFC3339),
		Projects:   []ProjectOutcome{},
	}
	if req.ForemanNote == "" {
		req.ForemanNote = fmt.Sprintf("deployed via scheduler template %s to group %s on %s", req.Template.Name, req.Group.Name, date)
	} else {
		req.ForemanNote += fmt.Sprintf(" — %s", date)
	}

	byName := make(map[string]ProjectTarget, len(req.Projects))
	for _, p := range req.Projects {
		if _, dup := byName[p.Name]; !dup {
			byName[p.Name] = p
		}
	}

	for _, member := range req.Group.Projects {
		o := ProjectOutcome{Project: member}
		target, known := byName[member]
		if !known || target.Workdir == "" {
			o.Workdir = target.Workdir
			o.Status = "error"
			o.Reason = fmt.Sprintf("project %q is not in the scheduler projects table (workdir unknown) — deploy targets are resolved from the projects DB at deploy time", member)
			res.Projects = append(res.Projects, o)
			continue
		}
		o.Workdir = target.Workdir
		rows := BuildTaskRows(req.Template, member, date, req.ForemanNote)
		if req.DryRun {
			plannedIDs := make([]string, 0, len(rows))
			for _, r := range rows {
				plannedIDs = append(plannedIDs, r.ID)
			}
			// Dry run still runs the read-only idempotency scan so the plan
			// matches what a live deploy would do — but never writes.
			skipped, reason, err := planAppend(target.Workdir, rows)
			if err != nil {
				o.Status = "error"
				o.Reason = err.Error()
			} else if skipped {
				o.Status = "would_skip"
				o.Reason = reason
			} else {
				o.Status = "would_append"
				o.TaskIDs = plannedIDs
			}
			res.Projects = append(res.Projects, o)
			continue
		}
		outcome, err := AppendTasks(target.Workdir, rows)
		if err != nil {
			o.Status = "error"
			o.Reason = err.Error()
		} else if outcome.Skipped {
			o.Status = "skipped"
			o.Reason = outcome.SkipReason
		} else {
			o.Status = "appended"
			o.TaskIDs = outcome.Appended
		}
		res.Projects = append(res.Projects, o)
	}

	for _, o := range res.Projects {
		res.Summary.Projects++
		switch o.Status {
		case "appended", "would_append":
			res.Summary.Appended++
			res.Summary.TaskRows += len(o.TaskIDs)
		case "skipped", "would_skip":
			res.Summary.Skipped++
		case "error":
			res.Summary.Errors++
		}
	}
	if len(res.Projects) == 0 {
		res.Summary.Appended = 0
	}
	return res
}

// DeployErrorDetail renders a compact per-project status list for event-log
// details JSON (one entry per deploy, per the API contract).
func DeployErrorDetail(res DeployResult) string {
	var sb strings.Builder
	sb.WriteString("{")
	fmt.Fprintf(&sb, "\"group\":%q,\"template\":%q,\"dry_run\":%v,", res.Group, res.Template, res.DryRun)
	sb.WriteString("\"projects\":{")
	for i, o := range res.Projects {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%q:%q", o.Project, o.Status)
	}
	sb.WriteString("}")
	sb.WriteString("}")
	return sb.String()
}
