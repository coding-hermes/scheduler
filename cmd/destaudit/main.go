// Command destaudit prints a READ-ONLY, sanitized inventory of every scheduler
// project's delivery destination against the approved group destination
// policies (SCHED-GAP-17084).
//
// It answers the audit question the fleet keeps re-asking by hand: for each
// enabled lane, which group owns it, is that group's mapping approved, and does
// the lane's configured target agree with the approval? It classifies; it never
// changes anything:
//
//   - the scheduler DB is opened with mode=ro (database.OpenReadOnly), so it
//     cannot take a write lock from the live daemon;
//   - groups.jsonl is only ever read (blocks.Store.ListGroups);
//   - no destination VALUE is printed. Each distinct target is reduced to a
//     reference (T1, T2, …) plus an 8-hex SHA-256 fingerprint, which keeps runs
//     comparable without publishing a chat id, thread id or token.
//
// Usage:
//
//	destaudit --db ~/.hermes/coding-hermes/scheduler.db \
//	          [--groups <path, default <db dir>/groups.jsonl>] [--all] [--json]
//
// --all includes disabled lanes (default: enabled only).
//
// Exit codes: 0 = every lane is classified and none is refused; 1 = at least
// one lane's target is refused by the delivery guard (drift, conflict,
// unapproved exception, invalid approval) or a lane expected by a policy is
// missing from the DB; 2 = usage / IO error.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

// auditRow is one lane's audit line. Target holds the raw destination for
// fingerprinting and is NEVER printed.
type auditRow struct {
	Project   string `json:"project"`
	Namespace string `json:"namespace_id,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Status    string `json:"status"`
	Allowed   bool   `json:"delivery_allowed"`
	Ref       string `json:"destination_ref,omitempty"`
	Target    string `json:"-"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// printf writes one report line. A write error on stdout/stderr is not
// actionable mid-report (and the process exit code carries the audit verdict),
// so it is deliberately discarded here rather than at every call site.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// printLine writes one already-formatted block (the JSON body).
func printLine(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("destaudit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "scheduler database path (opened read-only)")
	groupsPath := fs.String("groups", "", "groups.jsonl path (default <db dir>/groups.jsonl)")
	includeDisabled := fs.Bool("all", false, "include disabled lanes")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dbPath == "" {
		printLine(stderr, "destaudit: --db is required (scheduler database path)")
		return 2
	}
	if *groupsPath == "" {
		*groupsPath = filepath.Join(filepath.Dir(*dbPath), "groups.jsonl")
	}

	store := blocks.NewStore(*groupsPath, filepath.Join(filepath.Dir(*groupsPath), "templates.jsonl"))
	groups, err := store.ListGroups()
	if err != nil {
		printf(stderr, "destaudit: read groups %s: %v\n", *groupsPath, err)
		return 2
	}
	db, err := database.OpenReadOnly(*dbPath)
	if err != nil {
		printf(stderr, "destaudit: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()
	projects, err := database.ListProjects(context.Background(), db, !*includeDisabled)
	if err != nil {
		printf(stderr, "destaudit: list projects: %v\n", err)
		return 2
	}

	rows := buildRows(groups, projects)
	if *asJSON {
		out := struct {
			Groups   int            `json:"groups"`
			Policies int            `json:"approved_policies"`
			Projects int            `json:"projects"`
			Rows     []auditRow     `json:"rows"`
			Summary  map[string]int `json:"status_counts"`
			ExitCode int            `json:"exit_code"`
		}{
			Groups:   len(groups),
			Policies: approvedPolicies(groups),
			Projects: len(projects),
			Rows:     rows,
			Summary:  statusCounts(rows),
			ExitCode: auditExitCode(rows),
		}
		encoded, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			printf(stderr, "destaudit: encode: %v\n", err)
			return 2
		}
		printLine(stdout, string(encoded))
		return out.ExitCode
	}
	writeReport(stdout, groups, rows, *groupsPath, *dbPath, *includeDisabled)
	return auditExitCode(rows)
}

// buildRows classifies every project and stamps each row with a sanitized
// destination reference.
func buildRows(groups []blocks.Group, projects []database.Project) []auditRow {
	rows := make([]auditRow, 0, len(projects))
	for _, project := range projects {
		namespace := ""
		if project.NamespaceID != nil {
			namespace = *project.NamespaceID
		}
		decision := blocks.DecideDelivery(groups, project.Name, project.Deliver)
		rows = append(rows, auditRow{
			Project:   project.Name,
			Namespace: namespace,
			Origin:    decision.Origin,
			Status:    decision.Status,
			Allowed:   decision.Allowed,
			Target:    project.Deliver,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Project < rows[j].Project })
	referenceTargets(rows)
	return rows
}

// referenceTargets assigns stable T<n> references to distinct destinations, in
// first-appearance order of the (name-sorted) rows, and the 8-hex fingerprint of
// the value. Two rows share a reference iff they share the destination, so a
// divergence inside a family is visible without disclosing either value.
func referenceTargets(rows []auditRow) {
	refs := map[string]string{}
	next := 1
	for i := range rows {
		target := rows[i].Target
		if target == "" {
			rows[i].Ref = ""
			continue
		}
		ref, ok := refs[target]
		if !ok {
			ref = fmt.Sprintf("T%d/%s", next, fingerprint(target))
			next++
			refs[target] = ref
		}
		rows[i].Ref = ref
	}
}

// fingerprint is the first 8 hex characters of SHA-256 of the destination —
// enough to compare runs, not enough to reconstruct a chat id.
func fingerprint(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])[:8]
}

func approvedPolicies(groups []blocks.Group) int {
	count := 0
	for _, group := range groups {
		if group.DestinationPolicy != nil && group.DestinationPolicy.Approved {
			count++
		}
	}
	return count
}

func unapprovedPolicies(groups []blocks.Group) int {
	count := 0
	for _, group := range groups {
		if group.DestinationPolicy != nil && !group.DestinationPolicy.Approved {
			count++
		}
	}
	return count
}

func statusCounts(rows []auditRow) map[string]int {
	counts := map[string]int{}
	for _, row := range rows {
		counts[row.Status]++
	}
	return counts
}

// auditExitCode is 1 when any lane is REFUSED, i.e. the guard would stop its
// delivery. "unknown" is not a failure — no approved mapping owns the lane, so
// there is nothing to disagree with.
func auditExitCode(rows []auditRow) int {
	for _, row := range rows {
		if !row.Allowed {
			return 1
		}
	}
	return 0
}

func writeReport(w io.Writer, groups []blocks.Group, rows []auditRow, groupsPath, dbPath string, includeDisabled bool) {
	printf(w, "destination audit (SCHED-GAP-17084) — READ-ONLY\n")
	printf(w, "  groups:            %s (%d groups, %d approved policies, %d unapproved)\n",
		groupsPath, len(groups), approvedPolicies(groups), unapprovedPolicies(groups))
	printf(w, "  scheduler db:      %s (mode=ro)\n", dbPath)
	scope := "enabled only"
	if includeDisabled {
		scope = "enabled + disabled"
	}
	printf(w, "  lanes audited:     %d (%s)\n", len(rows), scope)
	printf(w, "  destination refs:  T<n>/<sha256:8> — values are never printed\n\n")

	counts := statusCounts(rows)
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	printf(w, "STATUS COUNTS\n")
	for _, status := range statuses {
		printf(w, "  %-12s %d\n", status, counts[status])
	}

	printf(w, "\nPER-LANE\n")
	printf(w, "  %-34s %-12s %-20s %-12s %-5s %s\n", "PROJECT", "NAMESPACE", "ORIGIN", "STATUS", "OK", "REF")
	for _, row := range rows {
		ok := "no"
		if row.Allowed {
			ok = "yes"
		}
		printf(w, "  %-34s %-12s %-20s %-12s %-5s %s\n",
			row.Project, orDash(row.Namespace), orDash(row.Origin), row.Status, ok, orDash(row.Ref))
	}

	// Shared destinations: the drift class this audit exists to expose — lanes
	// that a family mapping says share a thread, and lanes that do not.
	perRef := map[string][]string{}
	for _, row := range rows {
		if row.Ref == "" {
			continue
		}
		perRef[row.Ref] = append(perRef[row.Ref], row.Project)
	}
	refs := make([]string, 0, len(perRef))
	for ref := range perRef {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if len(perRef[refs[i]]) != len(perRef[refs[j]]) {
			return len(perRef[refs[i]]) > len(perRef[refs[j]])
		}
		return refs[i] < refs[j]
	})
	printf(w, "\nSHARED DESTINATIONS\n")
	for _, ref := range refs {
		printf(w, "  %-14s %3d lanes\n", ref, len(perRef[ref]))
	}

	refused := 0
	var unresolved []auditRow
	for _, row := range rows {
		if !row.Allowed {
			refused++
			unresolved = append(unresolved, row)
		}
	}
	printf(w, "\nUNRESOLVED DRIFT (%d)\n", refused)
	for _, row := range unresolved {
		printf(w, "  %s  status=%s origin=%s ref=%s\n", row.Project, row.Status, orDash(row.Origin), orDash(row.Ref))
	}
	if refused == 0 {
		printf(w, "  none\n")
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
