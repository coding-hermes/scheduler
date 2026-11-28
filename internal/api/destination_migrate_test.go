package api_test

// SCHED-GAP-17084 rework: HTTP-level tests for the approved destination
// migration APPLY and ROLLBACK verbs (POST /api/v1/groups/{name}/deploy with
// destination_apply / destination_rollback). Every write goes through the real
// handlers, the real blocks store on a temp groups.jsonl and the real
// projects.deliver writer, so these are end-to-end over the surface an operator
// actually calls, not unit tests of the engine (that is
// internal/blocks/destination_migrate_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/blocks"
	"github.com/coding-hermes/scheduler/internal/database"
)

const (
	apiMigrateGroup = "api-migrate"
	apiOldTarget    = "telegram:-100666:71"
	apiNewTarget    = "telegram:-100666:72"
	apiOtherTarget  = "telegram:-100666:73"
)

// mustCreateProjectWithTarget registers a project with a delivery target, the
// same column PUT /api/v1/projects writes.
func (b *blocksTestServer) mustCreateProjectWithTarget(t *testing.T, name, workdir, deliver string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), b.db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   workdir,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Deliver:   deliver,
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

func (b *blocksTestServer) projectDeliver(t *testing.T, name string) string {
	t.Helper()
	project, err := database.GetProject(context.Background(), b.db, name)
	if err != nil {
		t.Fatalf("GetProject %s: %v", name, err)
	}
	return project.Deliver
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return blocks.DestinationRef(string(data))
}

// applyFixture builds the HTTP fixture: one approved group with a drifting lane
// and an unset lane, plus an unrelated lane on its own target.
func applyFixture(t *testing.T) *blocksTestServer {
	t.Helper()
	b := newBlocksTestServer(t)
	b.mustCreateProjectWithTarget(t, "api-a", t.TempDir(), apiOldTarget)
	b.mustCreateProjectWithTarget(t, "api-b", t.TempDir(), "")
	b.mustCreateProjectWithTarget(t, "api-other", t.TempDir(), apiOtherTarget)
	b.mustCreateGroup(t, apiMigrateGroup, "api-a", "api-b")
	status, response := b.do(t, http.MethodPut, "/api/v1/groups/"+apiMigrateGroup, map[string]interface{}{
		"destination_policy": map[string]interface{}{"destination": apiNewTarget, "approved": true},
	})
	if status != http.StatusOK {
		t.Fatalf("set policy: %d %v", status, response)
	}
	return b
}

func applyBody() map[string]interface{} {
	return map[string]interface{}{
		"destination_apply": map[string]interface{}{"approvals": []interface{}{
			map[string]interface{}{"project": "api-a", "from": apiOldTarget, "to": apiNewTarget},
			map[string]interface{}{"project": "api-b", "from": "", "to": apiNewTarget},
		}},
	}
}

// TestDestinationApplyThroughTheAPI is the operator path end to end: a ready
// preview becomes an applied migration, the lanes really move, and nothing else
// about those project rows changes.
func TestDestinationApplyThroughTheAPI(t *testing.T) {
	b := applyFixture(t)
	before, err := database.GetProject(context.Background(), b.db, "api-a")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", applyBody())
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, response)
	}
	if response["applied"] != float64(2) || response["already_applied"] != float64(0) {
		t.Fatalf("apply response = %v", response)
	}
	digest, _ := response["plan_digest"].(string)
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("plan digest = %v", response["plan_digest"])
	}
	if b.projectDeliver(t, "api-a") != apiNewTarget || b.projectDeliver(t, "api-b") != apiNewTarget {
		t.Fatalf("lanes = %q/%q, want both on the approved destination",
			b.projectDeliver(t, "api-a"), b.projectDeliver(t, "api-b"))
	}
	if got := b.projectDeliver(t, "api-other"); got != apiOtherTarget {
		t.Fatalf("unrelated lane moved to %q", got)
	}

	// "without overwriting unrelated settings": every other column of the
	// moved project row is unchanged.
	after, err := database.GetProject(context.Background(), b.db, "api-a")
	if err != nil {
		t.Fatalf("GetProject after: %v", err)
	}
	if after.Weight != before.Weight || after.CooldownS != before.CooldownS ||
		after.Model != before.Model || after.Provider != before.Provider ||
		after.Enabled != before.Enabled || after.DecayRate != before.DecayRate {
		t.Fatalf("apply rewrote unrelated project settings:\nbefore=%+v\nafter=%+v", before, after)
	}
	if after.Deliver != apiNewTarget {
		t.Fatalf("deliver = %q", after.Deliver)
	}

	// The movement is in the ledger, and the ledger is not in any group body.
	stored, err := blocks.NewStore(b.groupsPath(), b.templatesPath()).GetGroup(apiMigrateGroup)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(stored.DestinationHistory) != 1 || stored.DestinationHistory[0].Kind != blocks.DestinationChangeApply {
		t.Fatalf("ledger = %+v", stored.DestinationHistory)
	}
	for _, path := range []string{"/api/v1/groups", "/api/v1/groups/" + apiMigrateGroup} {
		code, body := b.do(t, http.MethodGet, path, nil)
		encoded, _ := json.Marshal(body)
		if code != http.StatusOK || strings.Contains(string(encoded), "destination_history") ||
			strings.Contains(string(encoded), "telegram:") {
			t.Fatalf("group body %s leaks destination state: %d %s", path, code, encoded)
		}
	}
}

// TestDestinationApplyOverHTTPNeverLeaksDestinations asserts the sanitization
// contract on the wire: no chat id, no raw target, only refs and the digest.
func TestDestinationApplyOverHTTPNeverLeaksDestinations(t *testing.T) {
	b := applyFixture(t)
	status, response := b.doRaw(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", mustJSON(t, applyBody()))
	if status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, raw := range []string{apiOldTarget, apiNewTarget, "-100666"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("apply response leaked %q: %s", raw, encoded)
		}
	}
	moves, ok := response["moves"].([]interface{})
	if !ok || len(moves) != 2 {
		t.Fatalf("moves = %v", response["moves"])
	}
	for _, entry := range moves {
		move := entry.(map[string]interface{})
		if move["next_ref"] != blocks.DestinationRef(apiNewTarget) {
			t.Fatalf("next_ref = %v, want the sha256:8 fingerprint", move["next_ref"])
		}
		if _, has := move["next"]; has {
			t.Fatalf("response carried a raw destination: %v", move)
		}
	}
}

// TestDestinationApplyIsIdempotentOverHTTP proves the repeat apply is a no-op:
// zero writes and a byte-identical groups.jsonl.
func TestDestinationApplyIsIdempotentOverHTTP(t *testing.T) {
	b := applyFixture(t)
	if status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", applyBody()); status != http.StatusOK {
		t.Fatalf("first apply: %d %v", status, response)
	}
	firstDigest := fileSHA(t, b.groupsPath())

	status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", applyBody())
	if status != http.StatusOK {
		t.Fatalf("second apply: %d %v", status, response)
	}
	if response["applied"] != float64(0) || response["already_applied"] != float64(2) {
		t.Fatalf("second apply = %v, want applied=0 already=2", response)
	}
	if got := fileSHA(t, b.groupsPath()); got != firstDigest {
		t.Fatalf("repeat apply changed groups.jsonl: %s → %s", firstDigest, got)
	}
	stored, err := blocks.NewStore(b.groupsPath(), b.templatesPath()).GetGroup(apiMigrateGroup)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(stored.DestinationHistory) != 1 {
		t.Fatalf("repeat apply appended %d ledger records, want 1", len(stored.DestinationHistory))
	}
}

// TestDestinationRollbackThroughTheAPI is the reversibility proof over HTTP:
// both lanes go back to their captured values (including the unset one), the
// ledger records the rollback, and the policy is untouched.
func TestDestinationRollbackThroughTheAPI(t *testing.T) {
	b := applyFixture(t)
	if status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", applyBody()); status != http.StatusOK {
		t.Fatalf("apply: %d %v", status, response)
	}

	status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", map[string]interface{}{
		"destination_rollback": map[string]interface{}{},
	})
	if status != http.StatusOK {
		t.Fatalf("rollback: %d %v", status, response)
	}
	if response["rolled_back"] != float64(2) {
		t.Fatalf("rollback response = %v", response)
	}
	if got := b.projectDeliver(t, "api-a"); got != apiOldTarget {
		t.Fatalf("api-a = %q, want the captured prior target", got)
	}
	if got := b.projectDeliver(t, "api-b"); got != "" {
		t.Fatalf("api-b = %q, want the captured empty target", got)
	}
	if got := b.projectDeliver(t, "api-other"); got != apiOtherTarget {
		t.Fatalf("rollback touched an unrelated lane: %q", got)
	}

	stored, err := blocks.NewStore(b.groupsPath(), b.templatesPath()).GetGroup(apiMigrateGroup)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(stored.DestinationHistory) != 2 ||
		stored.DestinationHistory[1].Kind != blocks.DestinationChangeRollback {
		t.Fatalf("ledger = %+v", stored.DestinationHistory)
	}
	if stored.DestinationPolicy == nil || stored.DestinationPolicy.Destination != apiNewTarget || !stored.DestinationPolicy.Approved {
		t.Fatalf("rollback changed the approved policy: %+v", stored.DestinationPolicy)
	}
	// Fresh process shape: the restored state is what a restarted reader sees.
	// The restored lane is now DRIFT against the still-approved policy — rollback
	// reverses the migration, it does not withdraw the approval — so the guard
	// refuses it visibly instead of delivering somewhere undocumented.
	fresh, err := blocks.NewStore(b.groupsPath(), b.templatesPath()).ListGroups()
	if err != nil {
		t.Fatalf("fresh read: %v", err)
	}
	if decision := blocks.DecideDelivery(fresh, "api-a", apiOldTarget); decision.Allowed || decision.Status != blocks.DestinationDrift {
		t.Fatalf("restored lane classification = %+v (want drift: the approval still names the new target)", decision)
	}
}

// TestDestinationApplyRefusesAStalePlanOverHTTP is the required stale-plan red
// arm at the HTTP boundary: 409, a machine-readable code, and no writes.
func TestDestinationApplyRefusesAStalePlanOverHTTP(t *testing.T) {
	b := applyFixture(t)
	before := fileSHA(t, b.groupsPath())
	status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", map[string]interface{}{
		"destination_apply": map[string]interface{}{"approvals": []interface{}{
			map[string]interface{}{"project": "api-a", "from": apiOtherTarget, "to": apiNewTarget},
		}},
	})
	if status != http.StatusConflict {
		t.Fatalf("stale apply = %d %v, want 409", status, response)
	}
	if response["code"] != blocks.DestinationRefusedNotReady {
		t.Fatalf("code = %v, want %q", response["code"], blocks.DestinationRefusedNotReady)
	}
	refused, ok := response["refused"].([]interface{})
	if !ok || len(refused) != 1 || refused[0].(map[string]interface{})["status"] != "stale_approval" {
		t.Fatalf("refused = %v", response["refused"])
	}
	if got := b.projectDeliver(t, "api-a"); got != apiOldTarget {
		t.Fatalf("a refused apply moved the lane to %q", got)
	}
	if got := fileSHA(t, b.groupsPath()); got != before {
		t.Fatalf("a refused apply changed the store")
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "-100666") {
		t.Fatalf("refusal leaked a destination: %s", encoded)
	}
}

// TestDestinationRollbackRefusesWhenNothingCapturedOverHTTP is the required
// no-capture red arm at the HTTP boundary.
func TestDestinationRollbackRefusesWhenNothingCapturedOverHTTP(t *testing.T) {
	b := applyFixture(t)
	before := fileSHA(t, b.groupsPath())
	status, response := b.do(t, http.MethodPost, "/api/v1/groups/"+apiMigrateGroup+"/deploy", map[string]interface{}{
		"destination_rollback": map[string]interface{}{},
	})
	if status != http.StatusConflict {
		t.Fatalf("rollback without a capture = %d %v, want 409", status, response)
	}
	if response["code"] != blocks.DestinationRefusedNothingCaptred {
		t.Fatalf("code = %v, want %q", response["code"], blocks.DestinationRefusedNothingCaptred)
	}
	if got := b.projectDeliver(t, "api-a"); got != apiOldTarget {
		t.Fatalf("a refused rollback moved the lane to %q", got)
	}
	if got := fileSHA(t, b.groupsPath()); got != before {
		t.Fatalf("a refused rollback changed the store")
	}
}

// TestDestinationApplyRequiresAnApprovedPolicy pins the authority precondition
// at the HTTP boundary: an unapproved policy is not a licence to migrate.
func TestDestinationApplyRequiresAnApprovedPolicy(t *testing.T) {
	b := newBlocksTestServer(t)
	b.mustCreateProjectWithTarget(t, "api-a", t.TempDir(), apiOldTarget)
	b.mustCreateGroup(t, "api-unapproved", "api-a")
	if status, response := b.do(t, http.MethodPut, "/api/v1/groups/api-unapproved", map[string]interface{}{
		"destination_policy": map[string]interface{}{"destination": apiNewTarget, "approved": false},
	}); status != http.StatusOK {
		t.Fatalf("set policy: %d %v", status, response)
	}
	status, response := b.do(t, http.MethodPost, "/api/v1/groups/api-unapproved/deploy", map[string]interface{}{
		"destination_apply": map[string]interface{}{"approvals": []interface{}{
			map[string]interface{}{"project": "api-a", "from": apiOldTarget, "to": apiNewTarget},
		}},
	})
	if status != http.StatusConflict {
		t.Fatalf("unapproved apply = %d %v, want 409", status, response)
	}
	if got := b.projectDeliver(t, "api-a"); got != apiOldTarget {
		t.Fatalf("an unapproved policy migrated the lane to %q", got)
	}
}

// TestDestinationGroupCreateCannotInjectALedgerRecord pins the ingest guard: a
// create body carrying destination_history is dropped, so a forged rollback
// capture can never be planted through the group API.
func TestDestinationGroupCreateCannotInjectALedgerRecord(t *testing.T) {
	b := newBlocksTestServer(t)
	status, response := b.do(t, http.MethodPost, "/api/v1/groups", map[string]interface{}{
		"name":     "injected",
		"projects": []string{"api-a"},
		"destination_history": []interface{}{map[string]interface{}{
			"kind": "apply", "actor": "attacker", "at": "2026-11-28T00:00:00Z",
			"plan_digest": "sha256:0000000000000000",
			"moves": []interface{}{map[string]interface{}{
				"project": "api-a", "prior": "telegram:-1:1", "next": "telegram:-2:2",
			}},
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, response)
	}
	stored, err := blocks.NewStore(b.groupsPath(), b.templatesPath()).GetGroup("injected")
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if len(stored.DestinationHistory) != 0 {
		t.Fatalf("create body injected a ledger record: %+v", stored.DestinationHistory)
	}
}

func mustJSON(t *testing.T, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}
