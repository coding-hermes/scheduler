package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRegistry writes a fixture model table and returns its path.
func writeRegistry(t *testing.T, rows string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "models.jsonl")
	if err := os.WriteFile(p, []byte(rows), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	return p
}

// TestValidatePinsFlagsDisabledAndUnknown is the assertion CF-245 asks for,
// in both directions, on the exact pairs that were live on 2026-11-05:
// code-factory pinned deepseek-v4-flash/deepseek-foreman, a pair the registry
// has never contained, while the pair it should have named —
// deepseek-v4-pro/deepseek-foreman — is present and enabled.
func TestValidatePinsFlagsDisabledAndUnknown(t *testing.T) {
	path := writeRegistry(t, strings.Join([]string{
		`{"model":"deepseek-v4-pro","provider":"deepseek-foreman","disabled":false}`,
		`{"model":"deepseek-flash","provider":"deepseek-foreman","disabled":false}`,
		`{"model":"deepseek-v4-flash","provider":"ollama-cloud","disabled":true}`,
		``,
	}, "\n"))

	projects := []ProjectDef{
		// The shipped defect: model exists under OTHER providers, not this one.
		{Name: "code-factory", Model: "deepseek-v4-flash", Provider: "deepseek-foreman"},
		// The corrected pin: present and enabled -> must be clean.
		{Name: "temple-runner-foreman", Model: "deepseek-v4-pro", Provider: "deepseek-foreman"},
		// Explicitly disabled pair -> flagged as disabled, not unknown.
		{Name: "dead-lane", Model: "deepseek-v4-flash", Provider: "ollama-cloud"},
		// No pin at all is not a pin.
		{Name: "unpinned", Model: "", Provider: ""},
		// Half a pin cannot be resolved, so it is skipped rather than guessed.
		{Name: "half-pin", Model: "deepseek-v4-pro", Provider: ""},
	}

	reg, err := LoadPinRegistry(path)
	if err != nil {
		t.Fatalf("LoadPinRegistry: %v", err)
	}
	got := ValidatePins(projects, reg)

	if len(got) != 2 {
		t.Fatalf("got %d problem(s), want 2: %+v", len(got), got)
	}
	// Ordered by lane: code-factory, dead-lane.
	if got[0].Lane != "code-factory" || got[0].Reason != "not-in-registry" {
		t.Errorf("first problem = %+v, want code-factory not-in-registry", got[0])
	}
	if got[1].Lane != "dead-lane" || got[1].Reason != "disabled" {
		t.Errorf("second problem = %+v, want dead-lane disabled", got[1])
	}
	for _, p := range got {
		if p.Lane == "temple-runner-foreman" || p.Lane == "unpinned" || p.Lane == "half-pin" {
			t.Errorf("lane %q should not be flagged: %+v", p.Lane, p)
		}
	}
	if !strings.Contains(got[0].String(), "not-in-registry") {
		t.Errorf("String() does not name the reason: %q", got[0].String())
	}
}

// TestLanePinProblemsNeverFailsOnAnAbsentRegistry pins the fail-open direction
// deliberately: the registry belongs to another repository, and a host that
// runs only the scheduler must still start. A validator that errored here would
// trade a log warning for a fleet outage.
func TestLanePinProblemsNeverFailsOnAnAbsentRegistry(t *testing.T) {
	cfg := &RootConfig{Projects: []ProjectDef{
		{Name: "code-factory", Model: "deepseek-v4-flash", Provider: "deepseek-foreman"},
	}}
	missing := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	if got := LanePinProblems(cfg, missing); got != nil {
		t.Errorf("absent registry produced %d problem(s), want none: %+v", len(got), got)
	}

	// An empty file is "no registry" too, not "every pair is unknown" — the
	// distinction matters because an empty read would otherwise flag all 215
	// pinned lanes at once and train the operator to ignore the warning.
	empty := writeRegistry(t, "")
	if got := LanePinProblems(cfg, empty); got != nil {
		t.Errorf("empty registry produced %d problem(s), want none: %+v", len(got), got)
	}
}

// TestWarnUnservablePinsNamesTheLaneOnStderr proves the wiring, not just the
// pure function: a config whose pin cannot be served must reach stderr with the
// lane name, because that line is the only thing an operator sees at startup.
func TestWarnUnservablePinsNamesTheLaneOnStderr(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "models.jsonl")
	if err := os.WriteFile(reg, []byte(`{"model":"deepseek-v4-pro","provider":"deepseek-foreman","disabled":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHEDULER_PIN_REGISTRY", reg)

	cfg := &RootConfig{Projects: []ProjectDef{
		{Name: "code-factory", Model: "deepseek-v4-flash", Provider: "deepseek-foreman"},
	}}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	WarnUnservablePins(cfg, "")
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "code-factory") {
		t.Fatalf("stderr does not name the lane: %q", out)
	}
	if !strings.Contains(out, "deepseek-v4-flash/deepseek-foreman") {
		t.Errorf("stderr does not name the pair: %q", out)
	}
	if !strings.Contains(out, "not-in-registry") {
		t.Errorf("stderr does not name the reason: %q", out)
	}
}

// TestWarnUnservablePinsSilentWhenClean is the control for the test above: the
// same wiring must produce no output for a servable pin, or every startup log
// carries a warning the operator learns to skip.
func TestWarnUnservablePinsSilentWhenClean(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "models.jsonl")
	if err := os.WriteFile(reg, []byte(`{"model":"deepseek-v4-pro","provider":"deepseek-foreman","disabled":false}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHEDULER_PIN_REGISTRY", reg)

	cfg := &RootConfig{Projects: []ProjectDef{
		{Name: "code-factory", Model: "deepseek-v4-pro", Provider: "deepseek-foreman"},
	}}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	WarnUnservablePins(cfg, "")
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if n != 0 {
		t.Errorf("clean config produced stderr output: %q", string(buf[:n]))
	}
}
