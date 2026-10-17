package foreman_test

import (
	"errors"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/execution"
	"github.com/coding-hermes/scheduler/internal/foreman"
)

// TestRegressionChangedPolicyReplayConflicts proves the replay contract
// compares the full caller-authoritative attempt policy: a preparation of the
// same (parent, step, generation) whose StartTimeoutS/WallTimeoutS/
// BudgetUnits/ CancellationPolicy changed conflicts instead of silently
// replaying the stored attempt and swallowing the change.
func TestRegressionChangedPolicyReplayConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*execution.AttemptPolicy)
	}{
		{"start timeout 30 to 300", func(p *execution.AttemptPolicy) { p.StartTimeoutS = 300 }},
		{"wall timeout", func(p *execution.AttemptPolicy) { p.WallTimeoutS = 7200 }},
		{"budget units", func(p *execution.AttemptPolicy) { p.BudgetUnits = 250 }},
		{"cancellation policy", func(p *execution.AttemptPolicy) { p.CancellationPolicy = "immediate" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.admit(0)
			h.acquireLease("lease-1", 1, 0)
			h.invoke()
			intent := h.intent("step-1", 1, "lease-1", 1, h.route())
			intent.Policy.StartTimeoutS = 30
			stored := h.prepare("attempt-1", intent)
			if stored.Policy != intent.Policy {
				t.Fatalf("attempt policy not durable: stored=%+v intent=%+v", stored.Policy, intent.Policy)
			}

			changed := intent
			tc.mut(&changed.Policy)
			if changed.Policy == intent.Policy {
				t.Fatal("policy mutator left the policy unchanged")
			}
			if _, _, err := h.store.PrepareAttempt(h.ctx, "attempt-1", changed); !errors.Is(err, database.ErrExecutionConflict) {
				t.Fatalf("changed policy replayed instead of conflicting: error=%v", err)
			}
			if got := h.attemptCount(); got != 1 {
				t.Fatalf("changed policy replay mutated attempts: %d", got)
			}

			if _, replay, err := h.store.PrepareAttempt(h.ctx, "attempt-1", intent); err != nil || !replay {
				t.Fatalf("identical policy replay: replay=%v err=%v", replay, err)
			}
		})
	}
}

// TestRegressionPendingDispatchOutboxBlocksParentSettlement proves parent
// settlement requires terminal dispatch transport for every descendant: a
// pending or claimed execution.dispatch outbox row may still fire after the
// parent closes, so it refuses settlement until the dispatch is acknowledged
// or explicitly reconciled as undeliverable — and the row is never deleted.
func TestRegressionPendingDispatchOutboxBlocksParentSettlement(t *testing.T) {
	const transportWorker = "test-transport-worker"
	run := func(t *testing.T, resolve func(h *harness, m database.OutboxMessage)) {
		h := newHarness(t)
		h.admit(0)
		h.acquireLease("lease-1", 1, 0)
		receipt := h.invoke()
		attempt := h.prepare("attempt-1", h.intent("step-1", 1, "lease-1", 1, h.route()))
		if err := database.SetAttemptProcessIncarnation(h.ctx, h.db, attempt.AttemptID, attempt.Generation, 123, 123, "container-1"); err != nil {
			t.Fatal(err)
		}
		outcome, connector, independent := h.settleTriple()
		h.recordSettlementEvidence(attempt.AttemptID, 1, outcome, connector, independent)
		if _, err := h.store.SettleAttempt(h.ctx, foreman.AttemptSettlementRequest{Settlement: execution.AttemptSettlement{
			ParentRunID: h.parentID, AttemptID: attempt.AttemptID, Outcome: outcome, Connector: connector, Independent: independent,
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.RecordForemanReport(h.ctx, execution.ForemanReport{
			ParentRunID: h.parentID, InvocationID: receipt.InvocationID,
			Outcome: outcome, Connector: connector, Independent: independent,
		}); err != nil {
			t.Fatal(err)
		}
		settlement := execution.ParentSettlement{
			ParentRunID: h.parentID, Outcome: outcome, Connector: connector, Independent: independent,
			QuiescenceReceiptRef: "quiescence-1",
		}

		if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: settlement}); !errors.Is(err, execution.ErrTransportSettlementOutstanding) {
			t.Fatalf("pending dispatch settled the parent: error=%v", err)
		}

		m, err := database.ClaimExecutionOutbox(h.ctx, h.db, transportWorker)
		if err != nil {
			t.Fatal(err)
		}
		if m.AttemptID != attempt.AttemptID {
			t.Fatalf("claimed dispatch %s for attempt %s", m.OutboxID, m.AttemptID)
		}
		if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: settlement}); !errors.Is(err, execution.ErrTransportSettlementOutstanding) {
			t.Fatalf("claimed dispatch settled the parent: error=%v", err)
		}

		callerClaims := settlement
		callerClaims.Descendants = []execution.AttemptQuiescence{{
			AttemptID: attempt.AttemptID, Terminal: true, ConnectorSettled: true,
			ProcessBacked: true, QuiescenceRef: "quiescence-1", TransportSettled: true,
		}}
		if _, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: callerClaims}); !errors.Is(err, execution.ErrTransportSettlementOutstanding) {
			t.Fatalf("caller-supplied transport claim settled the parent: error=%v", err)
		}

		resolve(h, m)

		rec, err := h.store.SettleParent(h.ctx, foreman.ParentSettlementRequest{Settlement: settlement})
		if err != nil || rec.ParentState != execution.ParentSettled {
			t.Fatalf("parent=%+v err=%v", rec, err)
		}
		if got := h.outboxCount(); got != 1 {
			t.Fatalf("dispatch rows after settlement: %d, want 1 (never deleted)", got)
		}
		var state string
		if err := h.db.QueryRowContext(h.ctx, `SELECT state FROM execution_outbox WHERE outbox_id=?`, m.OutboxID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != "acked" && state != "dead" {
			t.Fatalf("dispatch state %q is not terminal transport", state)
		}
	}

	t.Run("acked dispatch settles the parent", func(t *testing.T) {
		run(t, func(h *harness, m database.OutboxMessage) {
			if err := database.AckExecutionOutbox(h.ctx, h.db, m.OutboxID, transportWorker, "ack-"+m.AttemptID); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("reconciled-undeliverable dispatch settles the parent", func(t *testing.T) {
		run(t, func(h *harness, m database.OutboxMessage) {
			const reason = "dispatch recipient gone before delivery"
			h.reconcileDispatchUndeliverable(m.AttemptID, reason)
			var state, lastError string
			if err := h.db.QueryRowContext(h.ctx, `SELECT state,last_error FROM execution_outbox WHERE outbox_id=?`, m.OutboxID).Scan(&state, &lastError); err != nil {
				t.Fatal(err)
			}
			if state != "dead" || lastError != reason {
				t.Fatalf("reconciled dispatch state=%q last_error=%q", state, lastError)
			}
			if err := database.ReconcileExecutionOutboxUndeliverable(h.ctx, h.db, m.OutboxID, reason); err != nil {
				t.Fatalf("identical reconciliation replay: %v", err)
			}
			if err := database.ReconcileExecutionOutboxUndeliverable(h.ctx, h.db, m.OutboxID, "different reason"); !errors.Is(err, database.ErrExecutionConflict) {
				t.Fatalf("changed reconciliation error=%v", err)
			}
		})
	})
}
