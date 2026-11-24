package scheduler

// CF-355 — the result-field mapping between a dispatch reply's `ok` and
// `status` members. Reference contract: code-factory specs/ENVELOPE.md —
// "`ok` is the verdict; `status` is the node's own classification":
// status "completed" requires ok=true; "failed"/"refused" require ok=false
// plus a reason; a contradicting pair is refused with the contradiction
// named; and a reply that carries NEITHER a boolean ok NOR a recognised
// status is a refusal too — silence is never recorded as success.
//
// The unit table pins the mapping directly; the end-to-end fixture proves the
// recorded tick outcome (a status:"failed" reply with no ok FAILS the tick).

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// TestDispatchReplyVerdict_Mapping pins the agreed result-field set: every
// legal shape, every contradiction, and the refusals.
func TestDispatchReplyVerdict_Mapping(t *testing.T) {
	cases := []struct {
		name       string
		payload    map[string]any
		wantOK     bool
		wantReason string // substring the refusal/failure reason must carry
	}{
		// The conforming shapes.
		{"ok true alone succeeds", map[string]any{"ok": true, "reply": "done"}, true, ""},
		{"ok true with status completed succeeds", map[string]any{"ok": true, "status": "completed", "reply": "done"}, true, ""},
		{"ok false with status failed carries the failure reason", map[string]any{"ok": false, "status": "failed", "error": "repo missing"}, false, "repo missing"},
		{"ok false with status refused is a failure", map[string]any{"ok": false, "status": "refused"}, false, "ok=false"},
		{"ok false decodes error_b64 into the reason", map[string]any{"ok": false, "error_b64": base64.StdEncoding.EncodeToString([]byte("boom"))}, false, "boom"},
		{"ok false alone stays the legacy failure", map[string]any{"ok": false}, false, "the agent reported ok=false"},

		// CF-355 refusals: silence and half-verdicts are never a pass.
		{"neither ok nor status is refused", map[string]any{"reply": "plain answer"}, false, "neither ok nor"},
		{"status failed with NO ok is FAIL", map[string]any{"status": "failed"}, false, "status=failed"},
		{"status refused with NO ok is FAIL", map[string]any{"status": "refused", "error": "gate denied"}, false, "gate denied"},
		{"status completed WITHOUT ok is refused", map[string]any{"status": "completed"}, false, "requires ok=true"},
		{"unrecognised status with NO ok is refused", map[string]any{"status": "banana"}, false, "neither ok nor"},
		{"non-boolean ok with NO status is refused", map[string]any{"ok": "true"}, false, "neither ok nor"},

		// CF-355 contradictions: status may not contradict the verdict.
		{"ok true contradicts status failed", map[string]any{"ok": true, "status": "failed"}, false, "contradicts"},
		{"ok true contradicts status refused", map[string]any{"ok": true, "status": "refused"}, false, "contradicts"},
		{"ok false contradicts status completed", map[string]any{"ok": false, "status": "completed"}, false, "contradicts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := dispatchReplyVerdict(tc.payload)
			if ok != tc.wantOK {
				t.Errorf("verdict ok = %v (reason %q), want %v", ok, why, tc.wantOK)
			}
			if !tc.wantOK && !strings.Contains(why, tc.wantReason) {
				t.Errorf("verdict reason = %q, want it to carry %q", why, tc.wantReason)
			}
			if tc.wantOK && why != "" {
				t.Errorf("verdict reason = %q, want empty on a success", why)
			}
		})
	}
}

// TestDispatchLeg_StatusFailedWithoutOkFailsTheTick — CF-355's end-to-end
// fixture: a reply that carries status:"failed" and NO ok member is recorded
// as a FAILED tick, never a silent pass, with the named refusal as the reason.
func TestDispatchLeg_StatusFailedWithoutOkFailsTheTick(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "")
	relay.agentStatus = "failed"
	_, pool, _, _, _ := newDispatchTestRig(t, relay, true)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "failed" {
		t.Fatalf("tick status = %q (error %q), want failed (the reply declared status=failed with no ok)", row.Status, row.Err)
	}
	if !strings.Contains(row.Err, "status=failed") {
		t.Errorf("tick error = %q, want the named status=failed refusal", row.Err)
	}
	// The receipt records REPLIED — an answer did correlate (dispatch_leg.go
	// keeps DispatchStateReplied whenever an answer arrives, regardless of
	// verdict; the tick status carries the failure) — and its reply carries
	// the named refusal, not the answer laundered as a completion.
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, ok, err)
	}
	if rec.State != database.DispatchStateReplied {
		t.Errorf("receipt state = %q, want replied (an answer correlated; the tick status carries the failure)", rec.State)
	}
	if !strings.Contains(rec.Reply, "status=failed") {
		t.Errorf("receipt reply = %q, want the named status=failed refusal", rec.Reply)
	}
}

// TestDispatchLeg_StatusCompletedWithoutOkIsRefused — the half-verdict arm:
// status "completed" without the ok=true it requires is refused with a named
// reason, never recorded as a completion.
func TestDispatchLeg_StatusCompletedWithoutOkIsRefused(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "")
	relay.agentStatus = "completed"
	_, pool, _, _, _ := newDispatchTestRig(t, relay, true)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "failed" {
		t.Fatalf("tick status = %q (error %q), want failed (completed requires ok=true, ok was absent)", row.Status, row.Err)
	}
	if !strings.Contains(row.Err, "requires ok=true") {
		t.Errorf("tick error = %q, want the named completed-requires-ok refusal", row.Err)
	}
}
