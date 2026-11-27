package blocks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDestinationMigrationPlanRequiresExactApproval(t *testing.T) {
	const group = "telegram:-123:10"
	const alternate = "telegram:-123:20"
	groups := []Group{
		{Name: "team:exception", Projects: []string{"ready", "stale", "duplicate", "exception", "pending", "conflict", "unapproved", "new"}},
		{Name: "other", Projects: []string{"conflict"}},
	}
	policies := map[string]DestinationPolicy{
		"team:exception": {Approved: true, Destination: group, Exceptions: map[string]DestinationException{
			"exception": {Approved: true, Destination: alternate},
			"pending":   {Destination: alternate},
		}},
		"other": {Approved: true, Destination: group},
	}
	projects := []DestinationProject{
		{Name: "ready", NamespaceID: "qa", Destination: alternate},
		{Name: "stale", NamespaceID: "docs", Destination: alternate},
		{Name: "duplicate", Destination: alternate},
		{Name: "exception", Destination: group},
		{Name: "pending", Destination: group},
		{Name: "conflict", Destination: alternate},
		{Name: "unknown", Destination: alternate},
		{Name: "unapproved", Destination: alternate},
		{Name: "new"},
	}
	approvals := []DestinationApproval{
		{Project: "ready", From: alternate, To: group},
		{Project: "stale", From: group, To: group},
		{Project: "duplicate", From: alternate, To: group},
		{Project: "duplicate", From: alternate, To: group},
		{Project: "exception", From: group, To: alternate},
		{Project: "pending", From: group, To: alternate},
		{Project: "conflict", From: alternate, To: group},
		{Project: "unknown", From: alternate, To: group},
		{Project: "new", From: "", To: group},
	}
	before, err := json.Marshal([]any{groups, policies, projects, approvals})
	if err != nil {
		t.Fatal(err)
	}
	items := PlanDestinationMigration(groups, policies, projects, approvals)
	want := map[string]string{"ready": "ready", "stale": "stale_approval", "duplicate": "ambiguous", "exception": "ready", "pending": "unknown", "conflict": "conflict", "unknown": "unknown", "unapproved": "approval_required", "new": "ready"}
	if len(items) != len(want) {
		t.Fatalf("items: %d, want %d", len(items), len(want))
	}
	for _, item := range items {
		if item.Status != want[item.Project] {
			t.Errorf("%s: got %s want %s", item.Project, item.Status, want[item.Project])
		}
	}
	after, err := json.Marshal([]any{groups, policies, projects, approvals})
	if err != nil || string(before) != string(after) {
		t.Fatal("plan mutated input")
	}
	output, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "telegram:") || strings.Contains(string(output), "-123") {
		t.Fatal("plan exposed destination")
	}
}

func TestDestinationMigrationPlanRejectsDuplicateProjects(t *testing.T) {
	items := PlanDestinationMigration([]Group{{Name: "g", Projects: []string{"p"}}}, map[string]DestinationPolicy{"g": {Approved: true, Destination: "telegram:123:1"}}, []DestinationProject{{Name: "p"}, {Name: "p"}}, []DestinationApproval{{Project: "p", To: "telegram:123:1"}})
	for _, item := range items {
		if item.Status != "ambiguous" {
			t.Fatalf("duplicate project accepted: %+v", item)
		}
	}
}
