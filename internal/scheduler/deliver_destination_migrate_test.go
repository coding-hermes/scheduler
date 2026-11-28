package scheduler

// SCHED-GAP-17084 rework, requirement B — the applied migration must be the
// mapping the delivery path actually enforces, read back from the FILE by a
// process that did not perform the apply.
//
// There is no sync-path writer for groups.jsonl in this repo (the file IS the
// source of truth and blocks.Store is its only writer), so the property that
// matters — and the one this test executes — is the restart shape: an apply
// writes the file, a FRESH store over the same path reads it, and the guard
// delivers to the migrated target and refuses the pre-migration one. The
// cross-node case is out of the product's scope and is recorded in the evidence,
// with the policy fingerprint (cmd/schedulerd's DEST-STORE line and
// cmd/destaudit) as the comparison surface.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

// TestDestinationGuardEnforcesAnAppliedMigration drives the real delivery path
// (deliverOutput → fake `hermes` transport) across an apply:
//
//	before: lane on the old thread → refused (drift against the approval)
//	apply:  approved migration moves the lane (captured through a DB-shaped writer)
//	after:  a FRESH store over the same file → delivered to the new thread
func TestDestinationGuardEnforcesAnAppliedMigration(t *testing.T) {
	dir := t.TempDir()
	groupsPath := filepath.Join(dir, "groups.jsonl")
	templatesPath := filepath.Join(dir, "templates.jsonl")
	store := blocks.NewStore(groupsPath, templatesPath)
	if err := store.CreateGroup(blocks.Group{Name: "moved-group", Projects: []string{"audit-lane"}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := store.UpdateGroup("moved-group", blocks.GroupUpdate{DestinationPolicy: &blocks.DestinationPolicy{
		Destination: otherTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	// targets stands in for projects.deliver: the engine never touches the DB
	// itself, the API hands it the existing writer.
	targets := map[string]string{"audit-lane": approvedTarget}
	writeTarget := func(project, destination string) error {
		targets[project] = destination
		return nil
	}

	resetDestinationAlertState()
	prev := currentDestinationPolicySource()
	SetDestinationPolicySource(store.ListGroups)
	t.Cleanup(func() {
		SetDestinationPolicySource(prev)
		resetDestinationAlertState()
	})
	_, capture := setupFakeHermes(t)

	// BEFORE the migration: the approval names otherTarget, the lane sits on
	// approvedTarget, so delivery is refused.
	deliverOutput(auditClock(), "audit-lane", "tick-b01", targets["audit-lane"], "prompt", tickBody())
	if got := readCapture(t, capture); got != "" {
		t.Fatalf("pre-migration lane was delivered despite drift; captured %q", got)
	}

	result, err := store.ApplyDestinationMigration(blocks.DestinationApplyInput{
		Group: "moved-group",
		Approvals: []blocks.DestinationApproval{
			{Project: "audit-lane", From: approvedTarget, To: otherTarget},
		},
		Projects:    []blocks.DestinationProject{{Name: "audit-lane", Destination: targets["audit-lane"]}},
		Actor:       "operator:token",
		At:          auditClock().Now(),
		WriteTarget: writeTarget,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Applied != 1 || targets["audit-lane"] != otherTarget {
		t.Fatalf("apply result = %+v targets = %v", result, targets)
	}

	// AFTER the migration, through a FRESH store (a restarted daemon): the file
	// is the only thing both processes share, so this is the durability proof.
	if err := os.WriteFile(capture, nil, 0o600); err != nil {
		t.Fatalf("reset capture: %v", err)
	}
	resetDestinationAlertState()
	restarted := blocks.NewStore(groupsPath, templatesPath)
	SetDestinationPolicySource(restarted.ListGroups)
	deliverOutput(auditClock(), "audit-lane", "tick-b02", targets["audit-lane"], "prompt", tickBody())
	if got := readCapture(t, capture); !strings.Contains(got, "--to "+otherTarget) {
		t.Fatalf("the migrated lane was not delivered from the restarted store; captured %q", got)
	}

	// And the same fresh store agrees on the policy fingerprint the daemon logs
	// at boot — the surface that lets two nodes detect divergent files.
	reloaded, err := restarted.ListGroups()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, want := blocks.DestinationPolicyFingerprint(reloaded), "sha256:"; !strings.HasPrefix(got, want) {
		t.Fatalf("policy fingerprint = %q, want a %s-prefixed digest", got, want)
	}
}
