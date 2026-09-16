package scheduler

import (
	"context"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-125 tests: the load-average admission gate. Bane 2026-09-16:
// "use the load average to decide if they should run or not — 16 cores, if
// the load average is below 12 maybe that is a good time to run things."

// T-LOAD-1: threshold 0 (the default) = gate fully disabled, defers nothing.
func TestLoadGateDisabledByDefault(t *testing.T) {
	SetLoadGateThreshold(0)
	defer SetLoadGateThreshold(0)
	if loadGateActive() {
		t.Fatal("gate active with threshold 0 — default must be off")
	}
	if LoadGateShouldDefer(nil, "coding-hermes") {
		t.Fatal("gate deferred with threshold 0 — must be a no-op")
	}
}

// T-LOAD-2: enabled gate + high load = defer; low load = admit.
func TestLoadGateDefersOnlyUnderLoad(t *testing.T) {
	// Inject a deterministic sampler so the test does not depend on the
	// host's actual load.
	orig := currentLoad1m
	defer func() { currentLoad1m = orig }()

	SetLoadGateThreshold(12)
	defer SetLoadGateThreshold(0)

	currentLoad1m = func() (float64, bool) { return 14.5, true }
	if !loadGateActive() {
		t.Fatal("load 14.5 >= threshold 12 — gate must be active")
	}
	if !LoadGateShouldDefer(nil, "coding-hermes") {
		t.Fatal("spawn should defer under load 14.5 vs threshold 12")
	}

	currentLoad1m = func() (float64, bool) { return 4.7, true }
	if loadGateActive() {
		t.Fatal("load 4.7 < threshold 12 — gate must be inactive")
	}
	if LoadGateShouldDefer(nil, "coding-hermes") {
		t.Fatal("spawn should NOT defer under load 4.7 vs threshold 12")
	}
}

// T-LOAD-3: missing telemetry fails OPEN (gate never blocks on absent data).
func TestLoadGateFailsOpenOnMissingTelemetry(t *testing.T) {
	orig := currentLoad1m
	defer func() { currentLoad1m = orig }()
	currentLoad1m = func() (float64, bool) { return 0, false }

	SetLoadGateThreshold(12)
	defer SetLoadGateThreshold(0)

	if loadGateActive() {
		t.Fatal("no reading available — gate must fail open")
	}
	if LoadGateShouldDefer(nil, "coding-hermes") {
		t.Fatal("no reading available — spawn must not be deferred")
	}
}

// T-LOAD-4: namespace opt-out (load_gate='off') exempts its projects.
func TestLoadGateNamespaceOptOut(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := database.CreateNamespace(ctx, db, &database.Namespace{ID: "infra-always-on", Weight: 10, Reserved: 1, HardCap: 100, MaxConcurrent: 2, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	off := "off"
	if err := database.UpdateNamespace(ctx, db, "infra-always-on", database.NamespacePatch{LoadGate: &off}); err != nil {
		t.Fatalf("UpdateNamespace load_gate=off: %v", err)
	}

	orig := currentLoad1m
	defer func() { currentLoad1m = orig }()
	currentLoad1m = func() (float64, bool) { return 15.0, true }

	SetLoadGateThreshold(12)
	defer SetLoadGateThreshold(0)

	// Namespace with gate ON (default ''): defers under load.
	if !LoadGateShouldDefer(db, "coding-hermes") {
		t.Fatal("default namespace should defer under load 15 vs threshold 12")
	}
	// Namespace with load_gate='off': never defers.
	if LoadGateShouldDefer(db, "infra-always-on") {
		t.Fatal("load_gate='off' namespace must never defer")
	}
	// Invalid value must be rejected by the write path.
	bad := "maybe"
	if err := database.UpdateNamespace(ctx, db, "infra-always-on", database.NamespacePatch{LoadGate: &bad}); err == nil {
		t.Fatal("invalid load_gate value must error (want \"off\" or \"\")")
	}
}

// T-LOAD-5: the DB round-trips load_gate through Get/Update.
func TestLoadGateRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := database.CreateNamespace(ctx, db, &database.Namespace{ID: "ns-l", Weight: 5, Reserved: 1, HardCap: 10, MaxConcurrent: 1, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	ns, err := database.GetNamespace(ctx, db, "ns-l")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.LoadGate != "" {
		t.Fatalf("fresh namespace load_gate = %q, want empty (gate applies)", ns.LoadGate)
	}
	off := "off"
	if err := database.UpdateNamespace(ctx, db, "ns-l", database.NamespacePatch{LoadGate: &off}); err != nil {
		t.Fatalf("UpdateNamespace: %v", err)
	}
	ns, err = database.GetNamespace(ctx, db, "ns-l")
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if ns.LoadGate != "off" {
		t.Fatalf("load_gate = %q after update, want \"off\"", ns.LoadGate)
	}
	// Clearing back to "" restores gate-applies.
	empty := ""
	if err := database.UpdateNamespace(ctx, db, "ns-l", database.NamespacePatch{LoadGate: &empty}); err != nil {
		t.Fatalf("UpdateNamespace clear: %v", err)
	}
	ns, _ = database.GetNamespace(ctx, db, "ns-l")
	if ns.LoadGate != "" {
		t.Fatalf("load_gate = %q after clear, want empty", ns.LoadGate)
	}
}
