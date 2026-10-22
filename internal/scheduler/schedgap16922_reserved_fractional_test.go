package scheduler

import (
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// ch:trace row=SCHED-GAP-16922 spec=none:owner-request-and-board-acceptance wave=.coding-hermes/waves/coding-hermes-scheduler-foreman-2026-10-22-21-23-23.json#SCHED-GAP-16922 test=internal/scheduler/*_test.go::ReservedFractional doc=none:implementation-docs-if-needed prompt=/tmp/sched-20261022212323-SCHED-GAP-16922-brief.txt evidence=commit witness=none:foreman-verifies-migration-and-test verdict=none:pending-foreman-trace commit=none:pending-foreman memory=none:not-written
// STATUS: complete
// TIER: T1 (focused ReservedFractional test, database/API/config/mcp suites, go build)
// EVIDENCE: commit for SCHED-GAP-16922; fractional floors accumulate before an integer slot is taken.
func TestReservedFractional(t *testing.T) {
	allocator := NewNamespaceAllocator(1)
	namespaces := []database.Namespace{
		{ID: "a-half", Weight: 1, Reserved: 0.5, Enabled: true},
		{ID: "b-half", Weight: 1, Reserved: 0.5, Enabled: true},
		{ID: "weight-only", Weight: 100, Enabled: true},
	}

	got := allocator.Allocate(namespaces)
	if got["a-half"]+got["b-half"] != 1 {
		t.Fatalf("two 0.5 reserved floors allocated %v, want one accumulated integer slot", got)
	}
	if got["a-half"] != 1 || got["b-half"] != 0 || got["weight-only"] != 0 {
		t.Fatalf("fractional floor allocation = %v, want a-half=1, b-half=0, weight-only=0", got)
	}
}
