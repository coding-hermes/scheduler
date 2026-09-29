package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SCHED-GAP-1674 acceptance 4 at the CONFIG layer: namespace-level
// overrides of the builder no-artifact guard's N and T parse, apply and
// validate; the documented defaults are 20m / 25 and live in the scheduler
// package (DefaultNoArtifactWindow / DefaultNoArtifactReconFloor); invalid
// values are rejected at load with field-named errors.

// writeFleetToml1674 writes a minimal fleet.toml with one namespace entry.
func writeFleetToml1674(t *testing.T, namespaceBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.toml")
	content := "[[namespaces]]\nid = \"test-ns\"\n" + namespaceBody
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	return path
}

func TestSCHEDGAP1674_FleetTomlGuardKnobsParse(t *testing.T) {
	path := writeFleetToml1674(t, `
reporter_class = "reporter"
no_artifact_window = "45m"
no_artifact_recon_floor = "40"
`)
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if len(cfg.Namespaces) != 1 {
		t.Fatalf("namespaces = %d, want 1", len(cfg.Namespaces))
	}
	ns := cfg.Namespaces[0]
	if ns.ReporterClass != "reporter" {
		t.Errorf("reporter_class = %q, want reporter", ns.ReporterClass)
	}
	if ns.NoArtifactWindow != "45m" {
		t.Errorf("no_artifact_window = %q, want 45m", ns.NoArtifactWindow)
	}
	if ns.NoArtifactReconFloor != "40" {
		t.Errorf("no_artifact_recon_floor = %q, want 40", ns.NoArtifactReconFloor)
	}
}

func TestSCHEDGAP1674_FleetTomlGuardKnobValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"bad reporter_class", "reporter_class = \"yes\"\n", "reporter_class"},
		{"bad window", "no_artifact_window = \"20 minutes\"\n", "no_artifact_window"},
		{"negative window", "no_artifact_window = \"-5m\"\n", "no_artifact_window"},
		{"bad floor", "no_artifact_recon_floor = \"lots\"\n", "no_artifact_recon_floor"},
		{"zero floor", "no_artifact_recon_floor = \"0\"\n", "no_artifact_recon_floor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFleetToml1674(t, tc.body)
			if _, err := LoadFleetConfig(path); err == nil {
				t.Fatalf("LoadFleetConfig accepted invalid %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name %q", err.Error(), tc.wantErr)
			}
		})
	}
}
