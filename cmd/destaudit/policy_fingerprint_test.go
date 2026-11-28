package main

// SCHED-GAP-17084 rework: the audit report and the daemon boot line must expose
// the SAME sanitized policy fingerprint, so an operator can tell two nodes apart
// without either of them printing a destination. This pins the report half.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

func TestReportPrintsThePolicyFingerprint(t *testing.T) {
	groups := []blocks.Group{{
		Name:              "alpha",
		Projects:          []string{"aligned"},
		DestinationPolicy: &blocks.DestinationPolicy{Destination: auditTargetA, Approved: true},
	}}
	projects := []database.Project{{Name: "aligned", Deliver: auditTargetA}}

	var out bytes.Buffer
	writeReport(&out, groups, buildRows(groups, projects), "groups.jsonl", "scheduler.db", false)
	report := out.String()

	want := blocks.DestinationPolicyFingerprint(groups)
	if !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("fingerprint = %q", want)
	}
	if !strings.Contains(report, "policy fingerprint: "+want) {
		t.Fatalf("report does not carry the policy fingerprint %q:\n%s", want, report)
	}
	for _, raw := range []string{auditTargetA, "-100999", "telegram:"} {
		if strings.Contains(report, raw) {
			t.Fatalf("report leaked %q", raw)
		}
	}
}

// TestFingerprintIgnoresUnapprovedPolicies keeps the comparison honest: only an
// APPROVED mapping is authoritative, so an unapproved (or absent) policy must
// not disturb the fingerprint two nodes compare.
func TestFingerprintIgnoresUnapprovedPolicies(t *testing.T) {
	approved := blocks.Group{
		Name:              "alpha",
		DestinationPolicy: &blocks.DestinationPolicy{Destination: auditTargetA, Approved: true},
	}
	withUnapproved := []blocks.Group{approved, {
		Name:              "beta",
		DestinationPolicy: &blocks.DestinationPolicy{Destination: auditTargetB, Approved: false},
	}}
	withoutIt := []blocks.Group{approved}
	if blocks.DestinationPolicyFingerprint(withUnapproved) != blocks.DestinationPolicyFingerprint(withoutIt) {
		t.Fatal("an unapproved policy changed the fingerprint")
	}

	// Flipping it to approved must change the fingerprint.
	withUnapproved[1].DestinationPolicy.Approved = true
	if blocks.DestinationPolicyFingerprint(withUnapproved) == blocks.DestinationPolicyFingerprint(withoutIt) {
		t.Fatal("granting an approval did not change the fingerprint")
	}
}
