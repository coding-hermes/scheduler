package api_test

// SCHED-GAP-17084: an approved destination_policy is authoritative for
// delivery, so an overwrite re-routes the lanes it owns. The PUT stays
// permitted (the API is the only writer of groups.jsonl) but must be named in
// the daemon log, without quoting a destination.

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
)

func captureAPILog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func putPolicy(t *testing.T, b *blocksTestServer, group string, policy map[string]interface{}) {
	t.Helper()
	status, response := b.do(t, http.MethodPut, "/api/v1/groups/"+group, map[string]interface{}{"destination_policy": policy})
	if status != http.StatusOK {
		t.Fatalf("policy PUT: %d %v", status, response)
	}
}

func TestGroupUpdateAlertsOnDestinationPolicyOverwrite(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateProject(t, "ow-a", t.TempDir())
	b.mustCreateGroup(t, "ow-group", "ow-a")
	logOut := captureAPILog(t)

	// Installing an approval for the first time is a policy, not an
	// overwrite — there was nothing to supersede.
	putPolicy(t, b, "ow-group", map[string]interface{}{"destination": "telegram:-123:10", "approved": true})
	if strings.Contains(logOut.String(), "DEST-OVERWRITE") {
		t.Fatalf("first approval must not be reported as an overwrite:\n%s", logOut.String())
	}

	// Re-stating the same policy is a no-op.
	putPolicy(t, b, "ow-group", map[string]interface{}{"destination": "telegram:-123:10", "approved": true})
	if strings.Contains(logOut.String(), "DEST-OVERWRITE") {
		t.Fatalf("an identical policy must not be reported as an overwrite:\n%s", logOut.String())
	}

	// Replacing the approved destination is the drift this alert exists for.
	putPolicy(t, b, "ow-group", map[string]interface{}{"destination": "telegram:-123:20", "approved": true})
	if !strings.Contains(logOut.String(), "DEST-OVERWRITE") {
		t.Fatalf("approved destination replacement was not alerted:\n%s", logOut.String())
	}
	if !strings.Contains(logOut.String(), "approved destination replaced") {
		t.Fatalf("overwrite alert did not name the change:\n%s", logOut.String())
	}
	if strings.Contains(logOut.String(), "-123") || strings.Contains(logOut.String(), "telegram:") {
		t.Fatalf("overwrite alert leaked a destination:\n%s", logOut.String())
	}

	// Withdrawing the approval disarms enforcement — also an overwrite.
	logOut.Reset()
	putPolicy(t, b, "ow-group", map[string]interface{}{"destination": "telegram:-123:20", "approved": false})
	if !strings.Contains(logOut.String(), "approval withdrawn") {
		t.Fatalf("approval withdrawal was not alerted:\n%s", logOut.String())
	}

	// The store really holds the withdrawn policy (a restart reads it back).
	fresh := blocks.NewStore(b.groupsPath(), b.templatesPath())
	group, err := fresh.GetGroup("ow-group")
	if err != nil || group.DestinationPolicy == nil || group.DestinationPolicy.Approved {
		t.Fatalf("withdrawn policy not persisted: %+v %v", group.DestinationPolicy, err)
	}
}
