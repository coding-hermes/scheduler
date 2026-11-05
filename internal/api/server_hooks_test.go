package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/lanehook"
)

// S13: the hook surface must answer from the FILE, not from a cache, and an
// unknown lane must 404 rather than resolving to an empty plan (an empty plan
// reads as "nothing to do", which is a different claim).
func TestHooksRoutesPlanAndMutate(t *testing.T) {
	db, err := database.InitDB(filepath.Join(t.TempDir(), "scheduler.db"))
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.toml")
	if err := os.WriteFile(path, []byte(`
[[hook]]
id = "g1"
phase = "pre"
level = "global"
target = "*"
priority = 10
command = "/bin/true"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := lanehook.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer(db, nil)
	s.SetHooks(set)
	s.SetAuthConfig(authConfig{mode: authToken, token: "test-token"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	get := func(u string) (int, string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			b.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		return resp.StatusCode, b.String()
	}

	code, body := get(ts.URL + "/api/v1/hooks")
	if code != 200 || !strings.Contains(body, `"g1"`) {
		t.Fatalf("GET /hooks = %d %s", code, body)
	}

	// unknown lane -> 404
	code, _ = get(ts.URL + "/api/v1/hooks/plan?lane=no-such-lane")
	if code != 404 {
		t.Fatalf("plan for unknown lane = %d, want 404", code)
	}

	// a known lane resolves the global step and nothing else
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
        VALUES ('demo-qa','local:/tmp/demo','/tmp/demo',datetime('now'),datetime('now'))`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	code, body = get(ts.URL + "/api/v1/hooks/plan?lane=demo-qa")
	if code != 200 || !strings.Contains(body, `"g1"`) {
		t.Fatalf("plan = %d %s", code, body)
	}

	// PUT creates a second step on disk
	bodyJSON := `{"phase":"pre","level":"lane","target":"demo-qa","priority":20,"command":"/bin/true","on_failure":"closed"}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/v1/hooks/wt-ensure", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var b strings.Builder
		buf := make([]byte, 2048)
		for {
			n, rerr := resp.Body.Read(buf)
			b.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		t.Fatalf("PUT = %d %s", resp.StatusCode, b.String())
	}
	var decoded map[string]any
	raw, _ := json.Marshal(map[string]any{})
	_ = json.Unmarshal(raw, &decoded)
	if len(set.Steps()) != 2 {
		t.Fatalf("after PUT the armed set has %d steps, want 2", len(set.Steps()))
	}

	// DELETE removes it again
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/hooks/wt-ensure", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || len(set.Steps()) != 1 {
		t.Fatalf("DELETE = %d, steps=%d", resp.StatusCode, len(set.Steps()))
	}

	// A bad body is refused and the file is untouched.
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/hooks/bad", strings.NewReader(`{"phase":"nope"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("invalid PUT = %d, want 400", resp.StatusCode)
	}
	if len(set.Steps()) != 1 {
		t.Fatalf("invalid PUT changed the armed set: %d steps", len(set.Steps()))
	}
}
