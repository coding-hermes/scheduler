package blocks

// SCHED-GAP-17084: the policy must survive a daemon restart (it is JSONL on
// disk, re-parsed on every read), a migration PREVIEW must leave the file
// byte-identical, and a rollback must restore the previous bytes exactly.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

const (
	persistTarget      = "telegram:-100666:51"
	persistNextTarget  = "telegram:-100666:52"
	persistStoreGroup  = "audit-persistence"
	persistStoreMember = "audit-lane"
)

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newPersistenceStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	groups := dir + "/groups.jsonl"
	store := NewStore(groups, dir+"/templates.jsonl")
	if err := store.CreateGroup(Group{Name: persistStoreGroup, Projects: []string{persistStoreMember}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return store, groups
}

// TestDestinationPolicyPersistsAcrossStoreInstances is the restart proof: a
// SECOND Store over the same paths (a fresh daemon process) resolves the policy
// from disk and the delivery guard therefore classifies the lane as aligned.
func TestDestinationPolicyPersistsAcrossStoreInstances(t *testing.T) {
	store, groupsPath := newPersistenceStore(t)
	_, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{DestinationPolicy: &DestinationPolicy{
		Destination: persistTarget,
		Approved:    true,
	}})
	if err != nil {
		t.Fatalf("set policy: %v", err)
	}

	restarted := NewStore(groupsPath, groupsPath+".templates")
	reloaded, err := restarted.ListGroups()
	if err != nil {
		t.Fatalf("reload groups: %v", err)
	}
	if len(reloaded) != 1 || reloaded[0].DestinationPolicy == nil ||
		reloaded[0].DestinationPolicy.Destination != persistTarget ||
		!reloaded[0].DestinationPolicy.Approved {
		t.Fatalf("policy did not survive a store restart: %+v", reloaded)
	}
	decision := DecideDelivery(reloaded, persistStoreMember, persistTarget)
	if !decision.Allowed || decision.Status != DestinationAligned {
		t.Fatalf("restarted policy did not classify the lane as aligned: %+v", decision)
	}
}

// TestDestinationPreviewLeavesStoreBytesUntouched proves the preview/plan path
// is non-destructive at the byte level, not merely at the API level.
func TestDestinationPreviewLeavesStoreBytesUntouched(t *testing.T) {
	store, groupsPath := newPersistenceStore(t)
	if _, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{DestinationPolicy: &DestinationPolicy{
		Destination: persistTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	before := fileDigest(t, groupsPath)

	groups, err := store.ListGroups()
	if err != nil {
		t.Fatal(err)
	}
	policies := map[string]DestinationPolicy{persistStoreGroup: *groups[0].DestinationPolicy}
	projects := []DestinationProject{{Name: persistStoreMember, Destination: persistNextTarget}}
	_ = InventoryDestinations(groups, policies, projects)
	plan := PlanDestinationMigration(groups, policies, projects, []DestinationApproval{{
		Project: persistStoreMember, From: persistNextTarget, To: persistTarget,
	}})
	if len(plan) != 1 || plan[0].Status != "ready" {
		t.Fatalf("preview plan = %+v, want one ready item", plan)
	}
	_ = DecideDelivery(groups, persistStoreMember, persistNextTarget)

	if after := fileDigest(t, groupsPath); after != before {
		t.Fatalf("a preview/plan pass changed the store bytes: before=%s after=%s", before, after)
	}
}

// TestDestinationPolicyRollbackRestoresBytes documents the ONLY rollback path
// this store supports today: the API has no "clear policy" verb
// (GroupUpdate.DestinationPolicy=nil means "leave untouched"), so a rollback is
// a file-level restore. That restore must land the EXACT prior bytes — including
// the previous policy — which is what this test proves.
func TestDestinationPolicyRollbackRestoresBytes(t *testing.T) {
	store, groupsPath := newPersistenceStore(t)
	if _, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{DestinationPolicy: &DestinationPolicy{
		Destination: persistTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	original, err := os.ReadFile(groupsPath)
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := fileDigest(t, groupsPath)

	// Forward migration (individually approved): rewrite the policy target.
	if _, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{DestinationPolicy: &DestinationPolicy{
		Destination: persistNextTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if fileDigest(t, groupsPath) == originalDigest {
		t.Fatal("migration did not change the store — nothing to roll back")
	}
	migrated, err := store.GetGroup(persistStoreGroup)
	if err != nil || migrated.DestinationPolicy.Destination != persistNextTarget {
		t.Fatalf("migrated policy not read back: %+v %v", migrated.DestinationPolicy, err)
	}

	// Rollback: restore the saved bytes.
	if err := os.WriteFile(groupsPath, original, 0o644); err != nil {
		t.Fatalf("rollback write: %v", err)
	}
	if after := fileDigest(t, groupsPath); after != originalDigest {
		t.Fatalf("rollback did not restore the store bytes: before=%s after=%s", originalDigest, after)
	}
	restored, err := NewStore(groupsPath, groupsPath+".templates").GetGroup(persistStoreGroup)
	if err != nil || restored.DestinationPolicy == nil || restored.DestinationPolicy.Destination != persistTarget {
		t.Fatalf("rolled-back policy not read back: %+v %v", restored.DestinationPolicy, err)
	}
}

// TestGroupUpdateCannotClearPolicy pins the recorded gap: a nil
// DestinationPolicy is "not supplied", so no PUT can clear an approval. If this
// test ever fails because a clear verb was added, update the finding.
func TestGroupUpdateCannotClearPolicy(t *testing.T) {
	store, groupsPath := newPersistenceStore(t)
	if _, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{DestinationPolicy: &DestinationPolicy{
		Destination: persistTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateGroup(persistStoreGroup, GroupUpdate{}); err != nil {
		t.Fatalf("empty update: %v", err)
	}
	reloaded, err := newPersistenceTestStore(groupsPath).GetGroup(persistStoreGroup)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DestinationPolicy == nil || !reloaded.DestinationPolicy.Approved {
		t.Fatalf("an empty update changed the policy: %+v", reloaded.DestinationPolicy)
	}
}

func newPersistenceTestStore(groupsPath string) *Store {
	return NewStore(groupsPath, groupsPath+".templates")
}
