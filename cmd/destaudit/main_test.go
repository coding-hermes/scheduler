package main

// SCHED-GAP-17084: the audit CLI's only job is to report — but it reports
// PRIVATE destinations, so the sanitization, reference assignment and exit-code
// rules are pinned here.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

const (
	auditTargetA = "telegram:-100999:41"
	auditTargetB = "telegram:-100999:42"
)

func namespacePtr(name string) *string { return &name }

func TestReferenceTargetsGroupsEqualDestinations(t *testing.T) {
	rows := []auditRow{
		{Project: "a", Target: auditTargetA},
		{Project: "b", Target: auditTargetB},
		{Project: "c", Target: auditTargetA},
		{Project: "d", Target: ""},
	}
	referenceTargets(rows)

	if rows[0].Ref != rows[2].Ref {
		t.Fatalf("identical destinations got different refs: %q vs %q", rows[0].Ref, rows[2].Ref)
	}
	if rows[0].Ref == rows[1].Ref {
		t.Fatalf("distinct destinations share a ref: %q", rows[0].Ref)
	}
	if rows[3].Ref != "" {
		t.Fatalf("an empty destination must not get a ref, got %q", rows[3].Ref)
	}
	if !strings.HasPrefix(rows[0].Ref, "T1/") || !strings.HasPrefix(rows[1].Ref, "T2/") {
		t.Fatalf("refs are not first-appearance ordered: %q %q", rows[0].Ref, rows[1].Ref)
	}
	if got, want := rows[0].Ref, "T1/"+fingerprint(auditTargetA); got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

func TestFingerprintIsShortAndStable(t *testing.T) {
	first := fingerprint(auditTargetA)
	if len(first) != 8 {
		t.Fatalf("fingerprint length = %d, want 8", len(first))
	}
	if first != fingerprint(auditTargetA) {
		t.Fatal("fingerprint is not deterministic")
	}
	if first == fingerprint(auditTargetB) {
		t.Fatal("two distinct destinations share a fingerprint")
	}
}

// TestReportNeverPrintsADestination is the sanitization gate: the report body
// must not contain a chat id or the "telegram:" prefix anywhere.
func TestReportNeverPrintsADestination(t *testing.T) {
	groups := []blocks.Group{{
		Name:     "alpha",
		Projects: []string{"aligned", "drifted"},
		DestinationPolicy: &blocks.DestinationPolicy{
			Destination: auditTargetA,
			Approved:    true,
		},
	}}
	projects := []database.Project{
		{Name: "aligned", NamespaceID: namespacePtr("docs"), Deliver: auditTargetA},
		{Name: "drifted", NamespaceID: namespacePtr("qa"), Deliver: auditTargetB},
		{Name: "unmapped", Deliver: auditTargetB},
	}
	rows := buildRows(groups, projects)

	var out bytes.Buffer
	writeReport(&out, groups, rows, "/fixtures/groups.jsonl", "/fixtures/scheduler.db", false)
	report := out.String()

	if strings.Contains(report, "telegram:") || strings.Contains(report, "100999") {
		t.Fatalf("report leaked a destination:\n%s", report)
	}
	if !strings.Contains(report, "aligned") || !strings.Contains(report, "drifted") {
		t.Fatalf("report is missing lane detail:\n%s", report)
	}
	if !strings.Contains(report, "UNRESOLVED DRIFT (1)") {
		t.Fatalf("report does not count the refused lane:\n%s", report)
	}
	if !strings.Contains(report, "SHARED DESTINATIONS") || !strings.Contains(report, "2 lanes") {
		t.Fatalf("report does not expose the shared destination:\n%s", report)
	}
}

func TestAuditExitCodeFailsOnlyOnRefusal(t *testing.T) {
	allowed := []auditRow{
		{Project: "a", Status: blocks.DestinationAligned, Allowed: true},
		{Project: "b", Status: blocks.DestinationUnknown, Allowed: true},
	}
	if got := auditExitCode(allowed); got != 0 {
		t.Fatalf("allowed rows exit code = %d, want 0", got)
	}
	refused := append([]auditRow{{Project: "c", Status: blocks.DestinationDrift}}, allowed...)
	if got := auditExitCode(refused); got != 1 {
		t.Fatalf("refused rows exit code = %d, want 1", got)
	}
}

func TestBuildRowsClassifiesAndSorts(t *testing.T) {
	groups := []blocks.Group{{
		Name:              "alpha",
		Projects:          []string{"aligned", "drifted"},
		DestinationPolicy: &blocks.DestinationPolicy{Destination: auditTargetA, Approved: true},
	}}
	projects := []database.Project{
		{Name: "unmapped", Deliver: auditTargetB},
		{Name: "drifted", Deliver: auditTargetB},
		{Name: "aligned", Deliver: auditTargetA, NamespaceID: namespacePtr("docs")},
	}
	rows := buildRows(groups, projects)

	if len(rows) != 3 || rows[0].Project != "aligned" || rows[1].Project != "drifted" || rows[2].Project != "unmapped" {
		t.Fatalf("rows are not name-sorted: %+v", rows)
	}
	if rows[0].Status != blocks.DestinationAligned || !rows[0].Allowed || rows[0].Namespace != "docs" {
		t.Fatalf("aligned row = %+v", rows[0])
	}
	if rows[1].Status != blocks.DestinationDrift || rows[1].Allowed {
		t.Fatalf("drifted row = %+v", rows[1])
	}
	if rows[2].Status != blocks.DestinationUnknown || !rows[2].Allowed {
		t.Fatalf("unmapped row = %+v", rows[2])
	}
}
