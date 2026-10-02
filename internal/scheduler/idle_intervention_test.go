package scheduler

import (
	"os"
	"strings"
	"testing"
)

// TestSCHEDGAP1688_IdleInterventionSwitch pins the global switch: the hook is
// OFF unless the env var is explicitly truthy, so it ships dark until it is
// proven on one family (the row's AC 5).
func TestSCHEDGAP1688_IdleInterventionSwitch(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"no", false}, {"off", false},
		{"garbage", false}, {"2", false},
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true}, {" ON ", true},
	} {
		t.Setenv(envIdleIntervention, tc.v)
		if got := idleInterventionFromEnv(); got != tc.want {
			t.Errorf("idleInterventionFromEnv() with %q = %v, want %v", tc.v, got, tc.want)
		}
	}
	os.Unsetenv(envIdleIntervention)
	if idleInterventionFromEnv() {
		t.Fatal("idle intervention must default OFF (no env var set)")
	}
}

// TestSCHEDGAP1688_PromptNamesLaneAndStores asserts the intervention prompt
// carries the lane, the tick and the summariser call — a lane that reads it
// knows exactly where to write the record (the row's in-session instruction).
func TestSCHEDGAP1688_PromptNamesLaneAndStores(t *testing.T) {
	p := idleInterventionPrompt("pulse", "T-42", "/home/kara/pulse")
	for _, want := range []string{
		"pulse", "T-42", "/home/kara/pulse",
		"duckbrain-session-summary.sh", "--kind noop",
		"GOOD outcome",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("intervention prompt missing %q:\n%s", want, p)
		}
	}
}

// TestSCHEDGAP1688_ResolutionChain pins AC5: lane > namespace > global, with an
// explicit OFF at any level honoured and an explicit ON able to arm a family
// while the global default is still OFF.
func TestSCHEDGAP1688_ResolutionChain(t *testing.T) {
	on := &Spawner{idleIntervention: true}
	for _, c := range []struct {
		lane, ns int
		want     bool
	}{
		{-1, -1, true},  // both inherit -> the global default
		{0, -1, false},  // lane OFF beats global ON
		{1, 0, true},    // lane ON beats namespace OFF
		{-1, 0, false},  // namespace OFF beats global ON
		{-1, 1, true},   // namespace ON
		{0, 1, false},   // lane OFF beats namespace ON
	} {
		got := on.resolveIdleIntervention(PackedProject{IdleIntervention: c.lane, NamespaceIdleIntervention: c.ns})
		if got != c.want {
			t.Errorf("global ON: resolve(lane=%d ns=%d) = %v, want %v", c.lane, c.ns, got, c.want)
		}
	}

	off := &Spawner{idleIntervention: false}
	if off.resolveIdleIntervention(PackedProject{IdleIntervention: -1, NamespaceIdleIntervention: -1}) {
		t.Error("global OFF + all inherit must stay OFF")
	}
	if !off.resolveIdleIntervention(PackedProject{IdleIntervention: 1, NamespaceIdleIntervention: -1}) {
		t.Error("lane ON must arm the hook even when the global default is OFF")
	}
	if !off.resolveIdleIntervention(PackedProject{IdleIntervention: -1, NamespaceIdleIntervention: 1}) {
		t.Error("namespace ON must arm the hook even when the global default is OFF")
	}
}
