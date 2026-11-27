package api_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

func TestDestinationPreviewUsesPersistedGroupPolicy(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateProject(t, "preview-a", t.TempDir())
	b.mustCreateProject(t, "preview-b", t.TempDir())
	b.mustCreateGroup(t, "preview-group", "preview-a", "preview-b")
	policy := map[string]interface{}{"destination": "telegram:-123:10", "approved": true}
	status, response := b.do(t, http.MethodPut, "/api/v1/groups/preview-group", map[string]interface{}{"destination_policy": policy})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, response)
	}
	for _, path := range []string{"/api/v1/groups", "/api/v1/groups/preview-group"} {
		code, result := b.do(t, http.MethodGet, path, nil)
		encoded, err := json.Marshal(result)
		if code != http.StatusOK || err != nil || strings.Contains(string(encoded), "telegram:") {
			t.Fatalf("policy exposed by %s: %d %s %v", path, code, encoded, err)
		}
	}
	// A fresh instance reads the same policy from the real JSONL store.
	fresh := blocks.NewStore(filepath.Join(b.storeDir, "groups.jsonl"), filepath.Join(b.storeDir, "templates.jsonl"))
	group, err := fresh.GetGroup("preview-group")
	if err != nil || group.DestinationPolicy == nil || group.DestinationPolicy.Destination != "telegram:-123:10" {
		t.Fatalf("reload: %+v %v", group, err)
	}
	b.mustCreateGroup(t, "preview-conflict", "preview-b")
	status, response = b.do(t, http.MethodPost, "/api/v1/groups/preview-group/deploy", map[string]interface{}{
		"destination_preview": map[string]interface{}{"approvals": []interface{}{
			map[string]interface{}{"project": "preview-a", "from": "", "to": "telegram:-123:10"},
			map[string]interface{}{"project": "preview-b", "from": "", "to": "telegram:-123:10"},
		}},
	})
	if status != http.StatusOK || response["dry_run"] != true {
		t.Fatalf("preview: %d %v", status, response)
	}
	items, ok := response["destination_plan"].([]interface{})
	if !ok || len(items) != 2 {
		t.Fatalf("items: %v", response)
	}
	states := map[string]string{}
	for _, item := range items {
		v := item.(map[string]interface{})
		states[v["project"].(string)] = v["status"].(string)
	}
	if states["preview-a"] != "ready" || states["preview-b"] != "conflict" {
		t.Fatalf("states: %v", states)
	}
	encoded, err := json.Marshal(response)
	if err != nil || strings.Contains(string(encoded), "telegram:") || strings.Contains(string(encoded), "-123") {
		t.Fatalf("leaked destination: %s %v", encoded, err)
	}
	status, response = b.do(t, http.MethodPost, "/api/v1/groups/preview-group/deploy", map[string]interface{}{
		"destination_preview": map[string]interface{}{"approvals": []interface{}{map[string]interface{}{"project": "preview-a", "from": "", "to": "telegram:-123:20"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("preview: %d %v", status, response)
	}
	items = response["destination_plan"].([]interface{})
	if items[0].(map[string]interface{})["status"] != "stale_approval" {
		t.Fatalf("wrong-thread approval: %v", response)
	}
}
