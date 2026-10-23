package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFleetConfigRejectsUnknownAdmissionMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "project",
			body: "[[projects]]\nname = \"mischief-chaos\"\nadmission_mode = \"task\"\n",
		},
		{
			name: "namespace",
			body: "[[namespaces]]\nid = \"mischief-chaos\"\nadmission_mode = \"task\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFleetConfig(path)
			if err == nil || !strings.Contains(err.Error(), `admission_mode "task" is invalid`) {
				t.Fatalf("LoadFleetConfig error = %v, want invalid admission_mode error", err)
			}
		})
	}
}

func TestLoadFleetConfigAcceptsSupportedAdmissionModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.toml")
	body := "[[projects]]\nname = \"satellite\"\nadmission_mode = \"cooldown\"\n" +
		"[[projects]]\nname = \"foreman\"\nadmission_mode = \"tasks\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFleetConfig(path); err != nil {
		t.Fatalf("LoadFleetConfig rejected supported admission modes: %v", err)
	}
}
