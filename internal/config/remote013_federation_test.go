package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemote013_FederationConfigLoads proves the [federation] TOML block
// decodes with the documented shape (REMOTE-013, federation-query-spec §4):
// the per-caller allow table — "*" the fleet-wide default plus specific
// caller ids — each carrying its published ops list. The daemon resolves
// this into api.ResolveFederationReadPolicy at boot; here we prove the TOML
// layer + the deny-all default (an absent block yields an empty allow map,
// which the api layer turns into the fail-closed policy).
func TestRemote013_FederationConfigLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedulerd.toml")
	content := `
[federation.allow."*"]
ops = ["fleet.status", "queue.get"]

[federation.allow."primary-01"]
ops = ["fleet.status"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	allow := cfg.Federation.Allow
	if len(allow) != 2 {
		t.Fatalf("allow rows = %d, want 2: %+v", len(allow), allow)
	}
	def, ok := allow["*"]
	if !ok {
		t.Fatalf("no \"*\" default row: %+v", allow)
	}
	if len(def.Ops) != 2 || def.Ops[0] != "fleet.status" || def.Ops[1] != "queue.get" {
		t.Errorf("default ops = %v", def.Ops)
	}
	prim, ok := allow["primary-01"]
	if !ok {
		t.Fatalf("no primary-01 row: %+v", allow)
	}
	if len(prim.Ops) != 1 || prim.Ops[0] != "fleet.status" {
		t.Errorf("primary-01 ops = %v", prim.Ops)
	}

	// Default config: NO federation grants — the deny-all posture is the
	// boot default (fail-closed; the api layer refuses every read until
	// the operator publishes ops).
	defCfg := defaultRootConfig()
	if len(defCfg.Federation.Allow) != 0 {
		t.Errorf("default [federation] allow = %+v, want empty (deny-all by default)", defCfg.Federation.Allow)
	}
}
