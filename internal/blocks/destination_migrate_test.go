package blocks

// SCHED-GAP-17084 rework: apply + rollback for an APPROVED destination
// migration. The preview could already say "ready"; nothing could act on it and
// the only rollback was a hand restore of groups.jsonl. These tests pin the
// four properties the row demands — idempotent, non-destructive, reversible,
// auditable — plus the two refusal arms (stale plan, nothing captured) and the
// persistence property that matters when there is no sync path: a FRESH store
// over the same file sees the applied state, and two stores over different
// files are distinguishable by their policy fingerprint instead of diverging
// silently.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	migrateGroup    = "audit-migrate"
	migrateOld      = "telegram:-100666:61"
	migrateNew      = "telegram:-100666:62"
	migrateOther    = "telegram:-100666:63"
	migrateActor    = "operator:token"
	migrateApplyAt  = "2026-11-28T03:00:00Z"
	migrateRollback = "2026-11-28T03:05:00Z"
)

var migrateApplyClock = time.Date(2026, 11, 28, 3, 0, 0, 0, time.UTC)
var migrateRollbackClock = time.Date(2026, 11, 28, 3, 5, 0, 0, time.UTC)

// targetWriter is the single writer the engine may use for a project's
// delivery target. It records every call in order so a test can assert both
// what was written and that a refusal wrote nothing at all.
type targetWriter struct {
	targets map[string]string // project → current value
	calls   []string          // "project=value" in call order
	failOn  string            // project whose write fails ("" = never)
}

func newTargetWriter(targets map[string]string) *targetWriter {
	copied := make(map[string]string, len(targets))
	for name, target := range targets {
		copied[name] = target
	}
	return &targetWriter{targets: copied}
}

func (w *targetWriter) write(project, destination string) error {
	if w.failOn != "" && project == w.failOn {
		return os.ErrPermission
	}
	w.targets[project] = destination
	w.calls = append(w.calls, project+"="+destination)
	return nil
}

func (w *targetWriter) reset() { w.calls = nil }

// migrateFixture builds a store with one approved group (two members: one
// drifting, one unset), an unrelated second group with its own approved policy,
// and a project view that matches.
func migrateFixture(t *testing.T) (*Store, string, []DestinationProject, *targetWriter) {
	t.Helper()
	dir := t.TempDir()
	groupsPath := filepath.Join(dir, "groups.jsonl")
	store := NewStore(groupsPath, filepath.Join(dir, "templates.jsonl"))
	policy := &DestinationPolicy{Destination: migrateNew, Approved: true}
	if err := store.CreateGroup(Group{Name: migrateGroup, Projects: []string{"mig-a", "mig-b"}, DestinationPolicy: policy}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := store.CreateGroup(Group{
		Name:              "unrelated",
		Projects:          []string{"other-a"},
		DestinationPolicy: &DestinationPolicy{Destination: migrateOther, Approved: true},
	}); err != nil {
		t.Fatalf("create unrelated group: %v", err)
	}
	projects := []DestinationProject{
		{Name: "mig-a", Destination: migrateOld},
		{Name: "mig-b", Destination: ""},
		{Name: "other-a", Destination: migrateOther},
	}
	return store, groupsPath, projects, newTargetWriter(map[string]string{
		"mig-a":   migrateOld,
		"mig-b":   "",
		"other-a": migrateOther,
	})
}

func migrateApprovals() []DestinationApproval {
	return []DestinationApproval{
		{Project: "mig-a", From: migrateOld, To: migrateNew},
		{Project: "mig-b", From: "", To: migrateNew},
	}
}

// storeLinesWithoutHistory canonicalizes every groups.jsonl line with the
// destination_history field removed, so "only the ledger changed" can be
// asserted byte-for-byte instead of field-by-field.
func storeLinesWithoutHistory(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]interface{}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		delete(record, "destination_history")
		canonical, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		lines = append(lines, string(canonical))
	}
	return lines
}

// TestDestinationApplyMovesLanesAndRecordsTheChange is the happy path: both an
// approved drift and an approved unset lane move, the prior value is captured
// for each, and the ledger record carries who/when/plan digest/moves.
func TestDestinationApplyMovesLanesAndRecordsTheChange(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)

	result, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group:       migrateGroup,
		Approvals:   migrateApprovals(),
		Projects:    projects,
		Actor:       migrateActor,
		At:          migrateApplyClock,
		WriteTarget: writer.write,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Applied != 2 || result.Already != 0 {
		t.Fatalf("applied=%d already=%d, want 2/0", result.Applied, result.Already)
	}
	if !strings.HasPrefix(result.PlanDigest, "sha256:") || len(result.PlanDigest) != len("sha256:")+16 {
		t.Fatalf("plan digest = %q, want sha256:<16 hex>", result.PlanDigest)
	}
	priors := map[string]string{}
	for _, move := range result.Moves {
		priors[move.Project] = move.Prior
		if move.Next != migrateNew {
			t.Fatalf("move %s next = %q, want the approved destination", move.Project, move.Next)
		}
	}
	if priors["mig-a"] != migrateOld {
		t.Fatalf("mig-a prior = %q, want the previous target captured", priors["mig-a"])
	}
	if priors["mig-b"] != "" {
		t.Fatalf("mig-b prior = %q, want the empty (unset) value captured", priors["mig-b"])
	}
	if writer.targets["mig-a"] != migrateNew || writer.targets["mig-b"] != migrateNew {
		t.Fatalf("writer targets = %v, want both lanes on the approved destination", writer.targets)
	}
	for _, call := range writer.calls {
		if strings.HasPrefix(call, "other-a=") {
			t.Fatalf("apply touched a lane outside the approved plan: %v", writer.calls)
		}
	}

	// The ledger must survive a re-read through the store (it is the rollback
	// capture, so losing it silently would make rollback impossible).
	reloaded, err := NewStore(groupsPath, groupsPath+".templates").GetGroup(migrateGroup)
	if err != nil {
		t.Fatalf("reload group: %v", err)
	}
	if len(reloaded.DestinationHistory) != 1 {
		t.Fatalf("history = %+v, want exactly one record", reloaded.DestinationHistory)
	}
	record := reloaded.DestinationHistory[0]
	if record.Kind != DestinationChangeApply || record.Actor != migrateActor ||
		record.At != migrateApplyAt || record.PlanDigest != result.PlanDigest {
		t.Fatalf("ledger record = %+v", record)
	}
	if len(record.Moves) != 2 {
		t.Fatalf("ledger moves = %+v, want 2", record.Moves)
	}
	if reloaded.DestinationPolicy == nil || reloaded.DestinationPolicy.Destination != migrateNew || !reloaded.DestinationPolicy.Approved {
		t.Fatalf("apply rewrote the policy: %+v", reloaded.DestinationPolicy)
	}
}

// TestDestinationApplyIsIdempotent proves the second apply of the same approved
// plan writes NOTHING: zero project writes and a byte-identical store.
func TestDestinationApplyIsIdempotent(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)

	first, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	afterFirst := fileDigest(t, groupsPath)
	applied, _ := store.ListGroups()
	writer.reset()

	second, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: appliedProjects(store, t),
		Actor: migrateActor, At: migrateApplyClock.Add(time.Minute), WriteTarget: writer.write,
	})
	if err != nil {
		t.Fatalf("second apply: %v (a repeat apply must be a no-op, not a refusal)", err)
	}
	if second.Applied != 0 || second.Already != 2 {
		t.Fatalf("second apply applied=%d already=%d, want 0/2", second.Applied, second.Already)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("second apply wrote %v, want no project writes", writer.calls)
	}
	if after := fileDigest(t, groupsPath); after != afterFirst {
		t.Fatalf("repeat apply changed the store: %s → %s", afterFirst, after)
	}
	if len(applied) != 2 {
		t.Fatalf("groups after apply = %d, want 2", len(applied))
	}
	if first.Applied != 2 {
		t.Fatalf("first apply applied=%d, want 2", first.Applied)
	}
}

// appliedProjects re-reads the delivered targets from the writer's own view by
// replaying the same fixture targets — the writer is the source of truth for
// the DB column, so the second apply must see what the writer now holds.
func appliedProjects(store *Store, t *testing.T) []DestinationProject {
	t.Helper()
	groups, err := store.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	var members []string
	for _, group := range groups {
		if group.Name == migrateGroup {
			members = group.Projects
		}
	}
	projects := make([]DestinationProject, 0, len(members))
	for _, name := range members {
		projects = append(projects, DestinationProject{Name: name, Destination: migrateNew})
	}
	projects = append(projects, DestinationProject{Name: "other-a", Destination: migrateOther})
	return projects
}

// TestDestinationApplyLeavesTheRestOfTheStoreUntouched is the non-destructive
// proof: every group row other than the ledger field, the templates file and
// every unrelated lane are byte-identical before and after.
func TestDestinationApplyLeavesTheRestOfTheStoreUntouched(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	templatesPath := filepath.Join(filepath.Dir(groupsPath), "templates.jsonl")
	if err := store.CreateTemplate(Template{Name: "keep-me", Tasks: []TemplateTask{{Title: "do not touch"}}}); err != nil {
		t.Fatalf("create template: %v", err)
	}
	before := storeLinesWithoutHistory(t, groupsPath)
	templatesBefore := fileDigest(t, templatesPath)

	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := storeLinesWithoutHistory(t, groupsPath)
	if len(after) != len(before) {
		t.Fatalf("group rows = %d, want %d", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("row %d changed outside the ledger:\nbefore: %s\nafter:  %s", i, before[i], after[i])
		}
	}
	if after := fileDigest(t, templatesPath); after != templatesBefore {
		t.Fatalf("templates file changed: %s → %s", templatesBefore, after)
	}
	// The unrelated lane must not have been written at all.
	for _, call := range writer.calls {
		if strings.HasPrefix(call, "other-a=") {
			t.Fatalf("unrelated lane written: %v", writer.calls)
		}
	}
}

// TestDestinationApplyRefusesAStalePlan is the required stale-plan red arm.
// Three ways a plan goes stale, each refused with zero writes.
func TestDestinationApplyRefusesAStalePlan(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	before := fileDigest(t, groupsPath)

	cases := []struct {
		name     string
		approval DestinationApproval
		status   string
	}{
		{"from-no-longer-matches", DestinationApproval{Project: "mig-a", From: migrateOther, To: migrateNew}, "stale_approval"},
		{"to-is-not-the-approved-target", DestinationApproval{Project: "mig-a", From: migrateOld, To: migrateOther}, "stale_approval"},
		{"project-is-not-a-member", DestinationApproval{Project: "ghost", From: "", To: migrateNew}, "not_a_member"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.ApplyDestinationMigration(DestinationApplyInput{
				Group: migrateGroup, Approvals: []DestinationApproval{tc.approval}, Projects: projects,
				Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
			})
			refusal, ok := IsDestinationRefusal(err)
			if !ok {
				t.Fatalf("apply err = %v, want a refusal", err)
			}
			if refusal.Code != DestinationRefusedNotReady {
				t.Fatalf("refusal code = %q, want %q", refusal.Code, DestinationRefusedNotReady)
			}
			if len(refusal.Items) != 1 || refusal.Items[0].Status != tc.status {
				t.Fatalf("refused items = %+v, want one %s", refusal.Items, tc.status)
			}
			if len(writer.calls) != 0 {
				t.Fatalf("a refused apply wrote %v", writer.calls)
			}
			if after := fileDigest(t, groupsPath); after != before {
				t.Fatalf("a refused apply changed the store: %s → %s", before, after)
			}
		})
	}
}

// TestDestinationApplyRefusesWithoutAnApprovedPolicy pins the authority
// precondition: an unapproved (or absent) policy authorizes nothing.
func TestDestinationApplyRefusesWithoutAnApprovedPolicy(t *testing.T) {
	dir := t.TempDir()
	groupsPath := filepath.Join(dir, "groups.jsonl")
	store := NewStore(groupsPath, filepath.Join(dir, "templates.jsonl"))
	if err := store.CreateGroup(Group{
		Name:              "unapproved-group",
		Projects:          []string{"mig-a"},
		DestinationPolicy: &DestinationPolicy{Destination: migrateNew, Approved: false},
	}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	writer := newTargetWriter(map[string]string{"mig-a": migrateOld})
	_, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: "unapproved-group", Approvals: []DestinationApproval{{Project: "mig-a", From: migrateOld, To: migrateNew}},
		Projects: []DestinationProject{{Name: "mig-a", Destination: migrateOld}},
		Actor:    migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	})
	refusal, ok := IsDestinationRefusal(err)
	if !ok || !strings.Contains(refusal.Reason, "APPROVED") {
		t.Fatalf("apply err = %v, want a refusal naming the missing approval", err)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("refused apply wrote %v", writer.calls)
	}
}

// TestDestinationRollbackRestoresPriorTargets is the reversibility proof: the
// exact captured priors come back (including the empty/unset lane), the ledger
// records the rollback, and the resolved mapping digest returns to its
// pre-apply value.
func TestDestinationRollbackRestoresPriorTargets(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	beforeDigest := destinationStateDigest(projects)

	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	migrated := map[string]string{"mig-a": writer.targets["mig-a"], "mig-b": writer.targets["mig-b"], "other-a": writer.targets["other-a"]}
	if destinationStateDigest(projectSlice(migrated)) == beforeDigest {
		t.Fatal("apply did not change the delivered mapping — nothing to roll back")
	}

	result, err := store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, Projects: projectSlice(migrated),
		Actor: migrateActor, At: migrateRollbackClock, WriteTarget: writer.write,
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if result.Restored != 2 {
		t.Fatalf("restored = %d, want 2", result.Restored)
	}
	if writer.targets["mig-a"] != migrateOld {
		t.Fatalf("mig-a = %q, want the captured prior target restored", writer.targets["mig-a"])
	}
	if writer.targets["mig-b"] != "" {
		t.Fatalf("mig-b = %q, want the captured empty target restored", writer.targets["mig-b"])
	}
	if got := destinationStateDigest(projectSlice(writer.targets)); got != beforeDigest {
		t.Fatalf("mapping digest after rollback = %s, want the pre-apply %s", got, beforeDigest)
	}

	reloaded, err := NewStore(groupsPath, groupsPath+".templates").GetGroup(migrateGroup)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.DestinationHistory) != 2 {
		t.Fatalf("history = %+v, want apply + rollback", reloaded.DestinationHistory)
	}
	rollback := reloaded.DestinationHistory[1]
	if rollback.Kind != DestinationChangeRollback || rollback.At != migrateRollback {
		t.Fatalf("rollback record = %+v", rollback)
	}
	if rollback.PlanDigest != reloaded.DestinationHistory[0].PlanDigest {
		t.Fatalf("rollback must name the digest it reverses: %q vs %q",
			rollback.PlanDigest, reloaded.DestinationHistory[0].PlanDigest)
	}
	if reloaded.DestinationPolicy == nil || reloaded.DestinationPolicy.Destination != migrateNew || !reloaded.DestinationPolicy.Approved {
		t.Fatalf("rollback changed the policy: %+v", reloaded.DestinationPolicy)
	}
}

// TestDestinationRollbackRefusesWhenNothingCaptured is the required
// no-capture red arm, in both shapes: never applied, and applied-then-already-
// rolled-back.
func TestDestinationRollbackRefusesWhenNothingCaptured(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	before := fileDigest(t, groupsPath)

	_, err := store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, Projects: projects,
		Actor: migrateActor, At: migrateRollbackClock, WriteTarget: writer.write,
	})
	refusal, ok := IsDestinationRefusal(err)
	if !ok || refusal.Code != DestinationRefusedNothingCaptred {
		t.Fatalf("rollback without a capture: err = %v, want code %q", err, DestinationRefusedNothingCaptred)
	}

	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	applied := projectSlice(map[string]string{"mig-a": migrateNew, "mig-b": migrateNew, "other-a": migrateOther})
	if _, err := store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, Projects: applied,
		Actor: migrateActor, At: migrateRollbackClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	rolledBack := projectSlice(writer.targets)
	digestAfterRollback := fileDigest(t, groupsPath)
	writer.reset()

	_, err = store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, Projects: rolledBack,
		Actor: migrateActor, At: migrateRollbackClock.Add(time.Minute), WriteTarget: writer.write,
	})
	refusal, ok = IsDestinationRefusal(err)
	if !ok || refusal.Code != DestinationRefusedNothingCaptred {
		t.Fatalf("second rollback: err = %v, want code %q", err, DestinationRefusedNothingCaptred)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("refused rollback wrote %v", writer.calls)
	}
	if after := fileDigest(t, groupsPath); after != digestAfterRollback {
		t.Fatalf("a refused rollback changed the store: %s → %s", digestAfterRollback, after)
	}
	if before == digestAfterRollback {
		t.Fatal("store never changed across apply+rollback — the test proves nothing")
	}
}

// TestDestinationRollbackRefusesDivergedLane proves rollback will not clobber a
// delivery target that an operator changed after the migration was applied.
func TestDestinationRollbackRefusesDivergedLane(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	diverged := projectSlice(map[string]string{"mig-a": migrateOther, "mig-b": migrateNew, "other-a": migrateOther})
	before := fileDigest(t, groupsPath)
	writer.reset()

	_, err := store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, Projects: diverged,
		Actor: migrateActor, At: migrateRollbackClock, WriteTarget: writer.write,
	})
	refusal, ok := IsDestinationRefusal(err)
	if !ok || refusal.Code != DestinationRefusedStateDiverged {
		t.Fatalf("rollback: err = %v, want code %q", err, DestinationRefusedStateDiverged)
	}
	if len(refusal.Lanes) != 1 || refusal.Lanes[0] != "mig-a" {
		t.Fatalf("diverged lanes = %v, want [mig-a]", refusal.Lanes)
	}
	if len(writer.calls) != 0 {
		t.Fatalf("refused rollback wrote %v", writer.calls)
	}
	if after := fileDigest(t, groupsPath); after != before {
		t.Fatalf("refused rollback changed the store: %s → %s", before, after)
	}
}

// TestDestinationRollbackPlanDigestMustMatch pins the optional digest guard: an
// operator reversing a specific migration cannot reverse a newer one by
// accident.
func TestDestinationRollbackPlanDigestMustMatch(t *testing.T) {
	store, _, projects, writer := migrateFixture(t)
	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	applied := projectSlice(map[string]string{"mig-a": migrateNew, "mig-b": migrateNew, "other-a": migrateOther})
	_, err := store.RollbackDestinationMigration(DestinationRollbackInput{
		Group: migrateGroup, PlanDigest: "sha256:0000000000000000", Projects: applied,
		Actor: migrateActor, At: migrateRollbackClock, WriteTarget: writer.write,
	})
	refusal, ok := IsDestinationRefusal(err)
	if !ok || refusal.Code != DestinationRefusedDigestMismatch {
		t.Fatalf("rollback: err = %v, want code %q", err, DestinationRefusedDigestMismatch)
	}
	if len(writer.calls) != 2 {
		t.Fatalf("writes = %d, want only the two apply writes", len(writer.calls))
	}
}

// TestDestinationApplyCompensatesAFailedLaneWrite proves a mid-batch failure
// leaves no half-migrated fleet: the already-written lanes are put back.
func TestDestinationApplyCompensatesAFailedLaneWrite(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	before := fileDigest(t, groupsPath)
	writer.failOn = "mig-b"

	_, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	})
	if err == nil {
		t.Fatal("apply succeeded although a lane write failed")
	}
	if writer.targets["mig-a"] != migrateOld {
		t.Fatalf("mig-a = %q, want the compensation to restore the captured prior value", writer.targets["mig-a"])
	}
	if after := fileDigest(t, groupsPath); after != before {
		t.Fatalf("a failed apply still wrote a ledger record: %s → %s", before, after)
	}
	if refusal, ok := IsDestinationRefusal(err); ok {
		t.Fatalf("an IO failure must not be reported as a refusal: %+v", refusal)
	}
}

// TestDestinationApplySurvivesAFreshStoreInstance is the persistence half of
// requirement B, executed against the property the product actually offers.
//
// There is NO sync path for groups.jsonl in this repo (the file IS the source of
// truth and the only writer is blocks.Store). What must hold — and what this
// test executes — is that the state written by an apply is what a FRESH process
// (a second Store over the same path, i.e. a restarted daemon) reads back and
// enforces, and that two stores over the same bytes agree on the policy
// fingerprint, so a node pointed at a different file is detectable instead of
// silently enforcing a different mapping.
func TestDestinationApplySurvivesAFreshStoreInstance(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	if _, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	fresh := NewStore(groupsPath, filepath.Join(filepath.Dir(groupsPath), "templates.jsonl"))
	reloaded, err := fresh.ListGroups()
	if err != nil {
		t.Fatalf("fresh store read: %v", err)
	}
	decision := DecideDelivery(reloaded, "mig-a", migrateNew)
	if !decision.Allowed || decision.Status != DestinationAligned {
		t.Fatalf("a restarted process did not enforce the applied mapping: %+v", decision)
	}
	// An empty lane still inherits the approved destination: the migration did
	// not have to guess a value.
	held := DecideDelivery(reloaded, "mig-b", "")
	if !held.Allowed || held.Status != DestinationInherited || held.Effective != migrateNew {
		t.Fatalf("fresh store inheritance = %+v", held)
	}

	original, err := store.ListGroups()
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if DestinationPolicyFingerprint(original) != DestinationPolicyFingerprint(reloaded) {
		t.Fatal("two stores over the same file disagree on the policy fingerprint")
	}
	// A node with a DIFFERENT file must be distinguishable from this one — that
	// is the observability half of the cross-node gap (there is no sync path).
	divergent := append([]Group{}, reloaded...)
	for i := range divergent {
		if divergent[i].Name == migrateGroup {
			divergent[i].DestinationPolicy = &DestinationPolicy{Destination: migrateOther, Approved: true}
		}
	}
	if DestinationPolicyFingerprint(divergent) == DestinationPolicyFingerprint(reloaded) {
		t.Fatal("a different approved mapping produced the same fingerprint")
	}
	if strings.Contains(DestinationPolicyFingerprint(divergent), "telegram") ||
		strings.Contains(DestinationPolicyFingerprint(reloaded), "telegram") {
		t.Fatal("the policy fingerprint leaked a destination value")
	}
}

// TestDestinationLedgerNeverRendersRawDestinations asserts the sanitization
// contract on every operator-visible projection of the ledger: the refs are
// sha256:8 and the JSON of the sanitized view carries no destination.
func TestDestinationLedgerNeverRendersRawDestinations(t *testing.T) {
	store, groupsPath, projects, writer := migrateFixture(t)
	result, err := store.ApplyDestinationMigration(DestinationApplyInput{
		Group: migrateGroup, Approvals: migrateApprovals(), Projects: projects,
		Actor: migrateActor, At: migrateApplyClock, WriteTarget: writer.write,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	view := SanitizeDestinationChange(result.Change)
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	for _, raw := range []string{migrateOld, migrateNew, "-100666"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("sanitized ledger leaked %q: %s", raw, encoded)
		}
	}
	for _, move := range view.Moves {
		if move.NextRef != DestinationRef(migrateNew) && move.NextRef != DestinationRef("") {
			t.Fatalf("next ref = %q, want the sha256:8 fingerprint", move.NextRef)
		}
	}
	// The store itself still holds the capture (that is what makes rollback
	// possible) — assert the split explicitly rather than implying it.
	reloaded, err := NewStore(groupsPath, groupsPath+".templates").GetGroup(migrateGroup)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.DestinationHistory[0].Moves[0].Prior == "" {
		t.Fatal("the rollback capture is missing from the store")
	}
}

// TestDestinationPolicyFingerprintIsStableAndSanitized pins the fingerprint
// helper's two properties: deterministic over the same mapping set, and empty
// of any destination value.
func TestDestinationPolicyFingerprintIsStableAndSanitized(t *testing.T) {
	groups := []Group{
		{Name: "b", DestinationPolicy: &DestinationPolicy{Destination: migrateNew, Approved: true}},
		{Name: "a", DestinationPolicy: &DestinationPolicy{
			Destination: migrateOld, Approved: true,
			Exceptions: map[string]DestinationException{"lane": {Destination: migrateOther, Approved: true}},
		}},
		{Name: "unapproved", DestinationPolicy: &DestinationPolicy{Destination: migrateOther, Approved: false}},
	}
	first := DestinationPolicyFingerprint(groups)
	reversed := []Group{groups[2], groups[1], groups[0]}
	if first != DestinationPolicyFingerprint(reversed) {
		t.Fatal("fingerprint depends on group order")
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+16 {
		t.Fatalf("fingerprint = %q", first)
	}
	changed := append([]Group{}, groups...)
	changed[0].DestinationPolicy = &DestinationPolicy{Destination: migrateOther, Approved: true}
	if DestinationPolicyFingerprint(changed) == first {
		t.Fatal("fingerprint does not change with the mapping")
	}
	if got := DestinationPolicyFingerprint(nil); got != DestinationPolicyFingerprint([]Group{}) {
		t.Fatalf("empty store fingerprint unstable: %q", got)
	}
}

// destinationStateDigest digests the resolved project → destination mapping, so
// "the store came back to exactly where it was" is a byte comparison, not a
// field-by-field spot check.
func destinationStateDigest(projects []DestinationProject) string {
	lines := make([]string, 0, len(projects))
	for _, project := range projects {
		lines = append(lines, project.Name+"="+DestinationRef(project.Destination))
	}
	sort.Strings(lines)
	return "state:" + strings.Join(lines, ";")
}

func projectSlice(targets map[string]string) []DestinationProject {
	projects := make([]DestinationProject, 0, len(targets))
	for name, target := range targets {
		projects = append(projects, DestinationProject{Name: name, Destination: target})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects
}
