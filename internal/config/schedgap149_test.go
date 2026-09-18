package config

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-149 durability tests for the namespace re-pin block.
//
// fleet.toml is the durable pin for a namespace's admission knobs: the loader
// re-applies it to the live SQLite row on every ApplyFleetConfig (the
// SCHED-GAP-025/121 law). Namespace max_concurrent was the one column wired
// only at CREATE time, so an operator cap written into fleet.toml was silently
// ignored after a restart whenever the namespace row already existed.
//
// Two properties are pinned here for all three keys:
//   - an entry that carries a value re-pins it over DB drift;
//   - an entry that carries no value never reverts the DB-only value
//     (the GatewayKey-conditional pattern).
//
// T-149-D3 additionally pins that a negative cap normalizes to 0 on the
// update path exactly as the create-time guard in namespaceFromDef does.
// The literal leak the row brief anticipated ("a -3 lands in the column")
// is not reachable at all: namespaces carries CHECK(max_concurrent >= 0)
// (internal/database/migrations.go:118), so the only two observable
// outcomes of a negative entry are "normalized to 0" or "the write failed
// and ApplyFleetConfig returned an error". D3 asserts the former and
// rejects the latter.

// T-149-D1: a fleet.toml entry carrying max_concurrent re-pins it over DB
// drift on the next load, and the pin is scoped to the keyed field only.
func TestSCHEDGAP149_MaxConcurrentPinsOverDBDrift(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{Namespaces: []NamespaceDef{{
		ID:            "ns-d1",
		MaxConcurrent: 4,
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-d1")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.MaxConcurrent != 4 {
		t.Fatalf("T-149-D1-0 FAIL: fleet.toml cap not stored at creation: got %d want 4", ns.MaxConcurrent)
	}

	// Simulate DB drift on the pinned field, plus a DB-only value on a
	// field the fleet.toml entry does NOT carry (DefaultPrompt is "" in
	// cfg) so the scoping half of the pin is observable too.
	if err := database.UpdateNamespace(ctx, db, "ns-d1", database.NamespacePatch{
		MaxConcurrent: intPtr(99),
		DefaultPrompt: strPtr("db-only-prompt"),
	}); err != nil {
		t.Fatalf("UpdateNamespace (drift): %v", err)
	}

	// Restart: the loader re-pins from fleet.toml.
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d1")
	if err != nil {
		t.Fatalf("GetNamespace (re-pin): %v", err)
	}
	if ns.MaxConcurrent != 4 {
		t.Errorf("T-149-D1 FAIL: max_concurrent drift survived the restart: got %d want 4", ns.MaxConcurrent)
	}
	if ns.DefaultPrompt != "db-only-prompt" {
		t.Errorf("T-149-D1b FAIL: pin was not scoped to the keyed field — the keyless DefaultPrompt was clobbered: got %q want %q",
			ns.DefaultPrompt, "db-only-prompt")
	}
}

// T-149-D2: a keyless fleet.toml entry never reverts a cap that lives only
// in the DB (an API-assigned max_concurrent survives the restart).
func TestSCHEDGAP149_KeylessEntryPreservesDBOnlyCap(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Keyless: only the id. MaxConcurrent is unset (== 0).
	cfg := &FleetConfig{Namespaces: []NamespaceDef{{ID: "ns-d2"}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-d2")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.MaxConcurrent != 0 {
		t.Fatalf("T-149-D2-0 FAIL: keyless entry did not create the namespace unlimited: got %d want 0", ns.MaxConcurrent)
	}

	// DB-only pin (no fleet.toml key).
	if err := database.UpdateNamespace(ctx, db, "ns-d2", database.NamespacePatch{
		MaxConcurrent: intPtr(7),
	}); err != nil {
		t.Fatalf("UpdateNamespace (db-only cap): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (restart): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d2")
	if err != nil {
		t.Fatalf("GetNamespace (restart): %v", err)
	}
	if ns.MaxConcurrent != 7 {
		t.Errorf("T-149-D2 FAIL: keyless fleet.toml entry reverted a DB-only max_concurrent: got %d want 7", ns.MaxConcurrent)
	}
}

// T-149-D3: a negative max_concurrent is normalized to 0 on the update path
// exactly as the create-time guard does, and never turns a boot into an error.
//
// The drift step is what makes this test non-vacuous: the create path alone
// normalizes (-3 -> 0), so a value check right after creation passes even with
// the pin block deleted. Resetting a DRIFTED cap back to 0 is only reachable
// through the update path.
func TestSCHEDGAP149_NegativeCapNormalizesToZero(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{Namespaces: []NamespaceDef{{
		ID:            "ns-d3",
		MaxConcurrent: -3,
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("T-149-D3 FAIL: ApplyFleetConfig errored on a negative cap (a typo must not break boot): %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-d3")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.MaxConcurrent != 0 {
		t.Errorf("T-149-D3a FAIL: create-time normalization lost: got %d want 0", ns.MaxConcurrent)
	}
	if ns.MaxConcurrent < 0 {
		t.Errorf("T-149-D3b FAIL: negative max_concurrent leaked: got %d want 0", ns.MaxConcurrent)
	}

	// Drift the row, then re-apply the same negative entry: the update path
	// must normalize the typo back to 0 (same input, same stored value on
	// both paths) rather than erroring on CHECK(max_concurrent >= 0).
	if err := database.UpdateNamespace(ctx, db, "ns-d3", database.NamespacePatch{
		MaxConcurrent: intPtr(5),
	}); err != nil {
		t.Fatalf("UpdateNamespace (drift): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("T-149-D3 FAIL: ApplyFleetConfig errored re-applying max_concurrent=-3: %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d3")
	if err != nil {
		t.Fatalf("GetNamespace (re-apply): %v", err)
	}
	if ns.MaxConcurrent != 0 {
		t.Errorf("T-149-D3 FAIL: negative max_concurrent did not normalize over drift: got %d want 0", ns.MaxConcurrent)
	}
}

// T-149-D4: load_gate re-pin coverage for the existing SCHED-GAP-125 behavior
// (shipped with no test until now).
func TestSCHEDGAP149_LoadGateRepinsAndIsKeylessSafe(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// namespaceFromDef does not read LoadGate, so creation leaves the column
	// at its schema default ("" = gate applies when enabled globally).
	keyless := &FleetConfig{Namespaces: []NamespaceDef{{ID: "ns-d4"}}}
	if err := ApplyFleetConfig(ctx, db, keyless); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-d4")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.LoadGate != "" {
		t.Fatalf("T-149-D4-0 FAIL: create-time load_gate should be the empty schema default: got %q", ns.LoadGate)
	}

	if err := database.UpdateNamespace(ctx, db, "ns-d4", database.NamespacePatch{
		LoadGate: strPtr("off"),
	}); err != nil {
		t.Fatalf("UpdateNamespace (db-only opt-out): %v", err)
	}

	// Keyless entry must not revert the DB-only opt-out.
	if err := ApplyFleetConfig(ctx, db, keyless); err != nil {
		t.Fatalf("ApplyFleetConfig (keyless restart): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d4")
	if err != nil {
		t.Fatalf("GetNamespace (keyless restart): %v", err)
	}
	if ns.LoadGate != "off" {
		t.Errorf("T-149-D4a FAIL: keyless fleet.toml entry reverted a DB-only load_gate: got %q want %q", ns.LoadGate, "off")
	}

	// Explicit pin wins over drift.
	if err := database.UpdateNamespace(ctx, db, "ns-d4", database.NamespacePatch{
		LoadGate: strPtr(""),
	}); err != nil {
		t.Fatalf("UpdateNamespace (drift to empty): %v", err)
	}
	pinned := &FleetConfig{Namespaces: []NamespaceDef{{ID: "ns-d4", LoadGate: "off"}}}
	if err := ApplyFleetConfig(ctx, db, pinned); err != nil {
		t.Fatalf("ApplyFleetConfig (pin): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d4")
	if err != nil {
		t.Fatalf("GetNamespace (pin): %v", err)
	}
	if ns.LoadGate != "off" {
		t.Errorf("T-149-D4b FAIL: load_gate pin did not beat drift: got %q want %q", ns.LoadGate, "off")
	}
}

// T-149-D5: namespace admission_mode re-pin coverage for the existing
// SCHED-GAP-124 behavior (shipped with no test until now).
func TestSCHEDGAP149_AdmissionModeRepinsAndIsKeylessSafe(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg := &FleetConfig{Namespaces: []NamespaceDef{{
		ID:            "ns-d5",
		AdmissionMode: database.AdmissionModeCooldown,
	}}}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-d5")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.AdmissionMode != database.AdmissionModeCooldown {
		t.Fatalf("T-149-D5-0 FAIL: fleet.toml admission_mode not stored at creation: got %q", ns.AdmissionMode)
	}

	// Drift, then re-pin from the same entry.
	if err := database.UpdateNamespace(ctx, db, "ns-d5", database.NamespacePatch{
		AdmissionMode: strPtr(database.AdmissionModeTasks),
	}); err != nil {
		t.Fatalf("UpdateNamespace (drift): %v", err)
	}
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig (re-pin): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d5")
	if err != nil {
		t.Fatalf("GetNamespace (re-pin): %v", err)
	}
	if ns.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-D5a FAIL: admission_mode drift survived the restart: got %q want %q", ns.AdmissionMode, database.AdmissionModeCooldown)
	}

	// Keyless entry must not revert the DB-only mode.
	if err := database.UpdateNamespace(ctx, db, "ns-d5", database.NamespacePatch{
		AdmissionMode: strPtr(database.AdmissionModeTasks),
	}); err != nil {
		t.Fatalf("UpdateNamespace (db-only mode): %v", err)
	}
	keyless := &FleetConfig{Namespaces: []NamespaceDef{{ID: "ns-d5"}}}
	if err := ApplyFleetConfig(ctx, db, keyless); err != nil {
		t.Fatalf("ApplyFleetConfig (keyless restart): %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-d5")
	if err != nil {
		t.Fatalf("GetNamespace (keyless restart): %v", err)
	}
	if ns.AdmissionMode != database.AdmissionModeTasks {
		t.Errorf("T-149-D5b FAIL: keyless fleet.toml entry reverted a DB-only admission_mode: got %q want %q", ns.AdmissionMode, database.AdmissionModeTasks)
	}
}

func intPtr(s int) *int { return &s }
