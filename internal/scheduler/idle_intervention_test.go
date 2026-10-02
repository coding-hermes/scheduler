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
