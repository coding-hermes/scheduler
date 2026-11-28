package scheduler

// SCHED-GAP-17084 — end-to-end proof that an APPROVED group destination policy
// is actually enforced on the real delivery path.
//
// These tests drive deliverOutputWithMode / deliverAlertWith — the same
// functions slot_pool calls when a tick completes — against the existing
// fake-`hermes` transport (setupFakeHermes, deliver_test.go). No network, no
// live daemon, no live settings: the transport is a PATH stub that records the
// argv of every `hermes send`.
//
// Every fixture destination below is synthetic ("-100777") and shares no value
// with the fleet's live threads.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/clock"
)

const (
	auditChat       = "-100777"
	approvedTarget  = "telegram:" + auditChat + ":11"
	otherTarget     = "telegram:" + auditChat + ":12"
	exceptionTarget = "telegram:" + auditChat + ":22"
	foreignTarget   = "telegram:" + auditChat + ":99"
)

func auditClock() clock.Clock {
	return clock.NewFixed(time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC))
}

// installDestinationPolicy arms the guard with a fixed group set and restores
// the previous source (plus the once-per-day gate) when the test ends.
func installDestinationPolicy(t *testing.T, groups []blocks.Group) {
	t.Helper()
	resetDestinationAlertState()
	prev := currentDestinationPolicySource()
	SetDestinationPolicySource(func() ([]blocks.Group, error) { return groups, nil })
	t.Cleanup(func() {
		SetDestinationPolicySource(prev)
		resetDestinationAlertState()
	})
}

// approvedGroup is a group whose policy was explicitly approved.
func approvedGroup(name string, destination string, members ...string) blocks.Group {
	return blocks.Group{
		Name:              name,
		Projects:          members,
		DestinationPolicy: &blocks.DestinationPolicy{Destination: destination, Approved: true},
	}
}

// TestDestinationGuardDeliversToApprovedTarget is the happy path: the lane's
// configured target IS the approved destination, so the report goes out.
func TestDestinationGuardDeliversToApprovedTarget(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "audit-lane")})
	_, capture := setupFakeHermes(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a1", approvedTarget, "prompt", tickBody())

	got := readCapture(t, capture)
	if !strings.Contains(got, "--to "+approvedTarget) {
		t.Fatalf("approved target was not delivered to; captured %q", got)
	}
}

// TestDestinationGuardInheritsGroupDestination proves lane inheritance: a lane
// with NO destination of its own delivers into its group's approved target
// instead of being dropped with "no delivery target configured".
func TestDestinationGuardInheritsGroupDestination(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "new-lane")})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "new-lane", "tick-a2", "", "prompt", tickBody())

	got := readCapture(t, capture)
	if !strings.Contains(got, "--to "+approvedTarget) {
		t.Fatalf("inherited delivery missing; captured %q", got)
	}
	if strings.Contains(logOut.String(), "no delivery target configured") {
		t.Fatalf("an inheriting lane must not report a missing target:\n%s", logOut.String())
	}
}

// TestDestinationGuardRefusesWrongThread is the wrong-thread negative control:
// the lane points one thread off its approved destination and NOTHING is sent.
func TestDestinationGuardRefusesWrongThread(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "audit-lane")})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a3", otherTarget, "prompt", tickBody())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("drifted target was delivered; captured %q", got)
	}
	if n := len(attemptLogLines(logOut.String())); n != 0 {
		t.Fatalf("a refused delivery must not retry: %d attempt line(s)\n%s", n, logOut.String())
	}
	if !strings.Contains(logOut.String(), destAlertDrift) || !strings.Contains(logOut.String(), "audit-lane") {
		t.Fatalf("drift alert missing:\n%s", logOut.String())
	}
	if strings.Contains(logOut.String(), auditChat) || strings.Contains(logOut.String(), "telegram:") {
		t.Fatalf("drift alert leaked a destination value:\n%s", logOut.String())
	}
}

// TestDestinationGuardHonorsApprovedException proves an APPROVED per-project
// exception is a legitimate route (it is what makes "wrong thread" only wrong
// when it was never approved).
func TestDestinationGuardHonorsApprovedException(t *testing.T) {
	group := approvedGroup("audit-group", approvedTarget, "audit-lane")
	group.DestinationPolicy.Exceptions = map[string]blocks.DestinationException{
		"audit-lane": {Destination: exceptionTarget, Approved: true},
	}
	installDestinationPolicy(t, []blocks.Group{group})
	_, capture := setupFakeHermes(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a4", exceptionTarget, "prompt", tickBody())

	if got := readCapture(t, capture); !strings.Contains(got, "--to "+exceptionTarget) {
		t.Fatalf("approved exception not delivered; captured %q", got)
	}
}

// TestDestinationGuardRefusesUnapprovedException: an exception nobody approved
// cannot authorize a route, even when the lane already points at it.
func TestDestinationGuardRefusesUnapprovedException(t *testing.T) {
	group := approvedGroup("audit-group", approvedTarget, "audit-lane")
	group.DestinationPolicy.Exceptions = map[string]blocks.DestinationException{
		"audit-lane": {Destination: exceptionTarget},
	}
	installDestinationPolicy(t, []blocks.Group{group})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a5", exceptionTarget, "prompt", tickBody())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("unapproved exception was delivered; captured %q", got)
	}
	if !strings.Contains(logOut.String(), destAlertUnapproved) {
		t.Fatalf("unapproved-exception alert missing:\n%s", logOut.String())
	}
}

// TestDestinationGuardRefusesConflictingOwnership: two groups claim the lane and
// one carries an approved policy, so ownership of the target cannot be
// established — the target is refused even though it matches that policy.
func TestDestinationGuardRefusesConflictingOwnership(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{
		approvedGroup("audit-group", approvedTarget, "audit-lane"),
		{Name: "second-group", Projects: []string{"audit-lane"}},
	})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a6", approvedTarget, "prompt", tickBody())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("conflicting ownership delivered; captured %q", got)
	}
	if !strings.Contains(logOut.String(), destAlertConflict) {
		t.Fatalf("conflict alert missing:\n%s", logOut.String())
	}
}

// TestDestinationGuardRefusesInvalidApproval: an approved policy whose value is
// not a telegram chat+thread pair cannot be inherited or matched.
func TestDestinationGuardRefusesInvalidApproval(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", "telegram:"+auditChat, "audit-lane")})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a7", approvedTarget, "prompt", tickBody())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("delivery proceeded on an invalid approved target; captured %q", got)
	}
	if !strings.Contains(logOut.String(), destAlertInvalid) {
		t.Fatalf("invalid-approval alert missing:\n%s", logOut.String())
	}
}

// TestDestinationGuardPassesThroughWithoutPolicy keeps the historical path
// byte-identical: a lane in a policy-less group (or in no group at all) is
// delivered exactly where its row says.
func TestDestinationGuardPassesThroughWithoutPolicy(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{
		{Name: "plain-group", Projects: []string{"audit-lane"}},
		{Name: "unapproved-group", Projects: []string{"other-lane"}, DestinationPolicy: &blocks.DestinationPolicy{Destination: approvedTarget}},
	})
	_, capture := setupFakeHermes(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a8", foreignTarget, "prompt", tickBody())
	if got := readCapture(t, capture); !strings.Contains(got, "--to "+foreignTarget) {
		t.Fatalf("policy-less lane was not delivered unchanged; captured %q", got)
	}

	deliverOutput(auditClock(), "other-lane", "tick-a9", foreignTarget, "prompt", tickBody())
	if got := readCapture(t, capture); !strings.Contains(got, "--to "+foreignTarget) {
		t.Fatalf("unapproved policy must not gate delivery; captured %q", got)
	}
}

// TestDestinationGuardDisabledWithoutSource pins the nil-seam behaviour every
// existing deployment relies on: with no source installed, even a target that
// would be drift is delivered.
func TestDestinationGuardDisabledWithoutSource(t *testing.T) {
	resetDestinationAlertState()
	SetDestinationPolicySource(nil)
	t.Cleanup(resetDestinationAlertState)
	_, capture := setupFakeHermes(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a10", otherTarget, "prompt", tickBody())

	if got := readCapture(t, capture); !strings.Contains(got, "--to "+otherTarget) {
		t.Fatalf("nil source must not gate delivery; captured %q", got)
	}
}

// TestDestinationGuardFailsOpenOnUnreadableStore: a broken policy file cannot
// be allowed to silence the fleet, but it must be loud.
func TestDestinationGuardFailsOpenOnUnreadableStore(t *testing.T) {
	resetDestinationAlertState()
	prev := currentDestinationPolicySource()
	SetDestinationPolicySource(func() ([]blocks.Group, error) { return nil, errors.New("groups.jsonl: boom") })
	t.Cleanup(func() {
		SetDestinationPolicySource(prev)
		resetDestinationAlertState()
	})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a11", otherTarget, "prompt", tickBody())

	if got := readCapture(t, capture); !strings.Contains(got, "--to "+otherTarget) {
		t.Fatalf("unreadable store must not block delivery; captured %q", got)
	}
	if !strings.Contains(logOut.String(), destAlertUnreadable) {
		t.Fatalf("unreadable-store alert missing:\n%s", logOut.String())
	}
}

// TestDestinationGuardAlertFiresOncePerUTCDay pins the gate: one line per
// project per UTC day, re-armed the next day, and never a destination value.
func TestDestinationGuardAlertFiresOncePerUTCDay(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "audit-lane")})
	_, capture := setupFakeHermes(t)
	logOut := captureDeliverLog(t)

	day1 := clock.NewFixed(time.Date(2026, 11, 28, 23, 59, 0, 0, time.UTC))
	day2 := clock.NewFixed(time.Date(2026, 11, 29, 0, 1, 0, 0, time.UTC))

	deliverOutput(day1, "audit-lane", "tick-a12", otherTarget, "prompt", tickBody())
	deliverOutput(day1, "audit-lane", "tick-a13", otherTarget, "prompt", tickBody())
	deliverOutput(day2, "audit-lane", "tick-a14", otherTarget, "prompt", tickBody())

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("refused deliveries reached the transport; captured %q", got)
	}
	if n := strings.Count(logOut.String(), destAlertDrift); n != 2 {
		t.Fatalf("want one drift alert per UTC day (2), got %d:\n%s", n, logOut.String())
	}
}

// TestDestinationGuardAppliesToAlerts closes the second delivery surface: a
// timeout alert must obey the same approved destination.
func TestDestinationGuardAppliesToAlerts(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "audit-lane")})
	_, capture := setupFakeHermes(t)

	deliverAlertWith(auditClock(), otherTarget, "audit-lane", "tick-a15", "timeout after 1m0s")
	if got := readCapture(t, capture); got != "" {
		t.Fatalf("alert delivered to a drifted target; captured %q", got)
	}

	deliverAlertWith(auditClock(), approvedTarget, "audit-lane", "tick-a16", "timeout after 1m0s")
	if got := readCapture(t, capture); !strings.Contains(got, "--to "+approvedTarget) {
		t.Fatalf("alert to the approved target was not delivered; captured %q", got)
	}
}

// TestDestinationGuardReadsTheGroupsJSONLStore closes the last wiring seam:
// the source cmd/schedulerd installs is `blocks.Store.ListGroups` over the same
// groups.jsonl the deploy-block API writes. Driving that real store through the
// real delivery path is the end-to-end proof — file → policy → transport.
func TestDestinationGuardReadsTheGroupsJSONLStore(t *testing.T) {
	dir := t.TempDir()
	groupsPath := filepath.Join(dir, "groups.jsonl")
	store := blocks.NewStore(groupsPath, filepath.Join(dir, "templates.jsonl"))
	if err := store.CreateGroup(blocks.Group{Name: "audit-group", Projects: []string{"audit-lane"}}); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := store.UpdateGroup("audit-group", blocks.GroupUpdate{DestinationPolicy: &blocks.DestinationPolicy{
		Destination: approvedTarget,
		Approved:    true,
	}}); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	resetDestinationAlertState()
	prev := currentDestinationPolicySource()
	SetDestinationPolicySource(store.ListGroups)
	t.Cleanup(func() {
		SetDestinationPolicySource(prev)
		resetDestinationAlertState()
	})
	_, capture := setupFakeHermes(t)

	deliverOutput(auditClock(), "audit-lane", "tick-a18", approvedTarget, "prompt", tickBody())
	if got := readCapture(t, capture); !strings.Contains(got, "--to "+approvedTarget) {
		t.Fatalf("approved lane was not delivered from the real store; captured %q", got)
	}

	// One thread over: the store's approval refuses it.
	if err := os.WriteFile(capture, nil, 0o600); err != nil {
		t.Fatalf("reset capture: %v", err)
	}
	deliverOutput(auditClock(), "audit-lane", "tick-a19", otherTarget, "prompt", tickBody())
	if got := readCapture(t, capture); got != "" {
		t.Fatalf("drifted target was delivered despite the store policy; captured %q", got)
	}
}

// TestDestinationGuardRefusalLeavesNoReportFile: refusing must happen BEFORE the
// mode work (temp file / attachment) so a refused tick leaves no artifact.
func TestDestinationGuardRefusalLeavesNoReportFile(t *testing.T) {
	installDestinationPolicy(t, []blocks.Group{approvedGroup("audit-group", approvedTarget, "audit-lane")})
	_, capture := setupFakeHermes(t)
	var body bytes.Buffer
	body.WriteString("tick report body\n\n_tick-a17 · prompt_\n")

	deliverOutputWithMode(auditClock(), "audit-lane", "tick-a17", otherTarget, "prompt", &body, DeliverModeFile)

	if got := readCapture(t, capture); got != "" {
		t.Fatalf("file-mode delivery on a drifted target reached the transport; captured %q", got)
	}
}
