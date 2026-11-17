package api_test

// SCHED-GAP-1765 — the smaller default family template.
//
// Owner ruling 2026-11-16 (704 lanes / 118 families; 445 enabled support lanes
// against 49 enabled foremen — 9:1 support against the 10% support-capacity
// doctrine): onboarding a project must mint foreman + qa + sync + docs, and
// every other role suffix (-pm -dogfood -perf -releng -review -readme) must be
// minted on demand by an owner or foreman decision.
//
// These tests drive the REAL handlers through the shared API stack
// (newAPITestServer: in-memory DB + armed operator gate), so a regression in the
// wiring — not just in the pure rule — fails here:
//
//   - the default set is exactly four lanes, and NONE of the six on-demand
//     roles is minted by an onboarding request;
//   - the on-demand path still mints each of those six suffixes, one at a time;
//   - a plain (non-family) create still mints exactly the one row it always
//     did, with the historical bare-Project 201 body;
//   - a satellite that already exists is REPORTED, not touched (the mint is
//     additive: no existing project row is read-rewritten or deleted).

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// gap1765FamilyBody is a family onboarding create body: a primary plus the
// family flag.
func gap1765FamilyBody(name string, extra map[string]interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"name":     name,
		"repo_url": "local:/tmp/" + name,
		"workdir":  "/tmp/" + name,
		"family":   true,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// gap1765Count projects counts the projects table — the "exactly N rows were
// written" assertion the mint needs (an absence check per name cannot catch an
// EXTRA row nobody named).
func gap1765Count(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	return n
}

// gap1765Strings converts a decoded JSON array into []string.
func gap1765Strings(t *testing.T, v interface{}) []string {
	t.Helper()
	arr, ok := v.([]interface{})
	if !ok {
		t.Fatalf("value is not a JSON array: %#v", v)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("array element is not a string: %#v", item)
		}
		out = append(out, s)
	}
	return out
}

// gap1765FamilyReport pulls the family block out of a 201 body.
func gap1765FamilyReport(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	fam, ok := body["family"].(map[string]interface{})
	if !ok {
		t.Fatalf("response carries no family block: %#v", body)
	}
	return fam
}

// gap1765SeedNamespaces registers the family namespaces the default template
// assigns, so the minted lanes land in the live family shape rather than falling
// back to a null namespace (the fallback has its own test below).
func gap1765SeedNamespaces(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := database.CreateNamespace(context.Background(), db, &database.Namespace{
			ID: id, Weight: 10, Enabled: true,
		}); err != nil {
			t.Fatalf("seed namespace %q: %v", id, err)
		}
	}
}

// TestSCHEDGAP1765_FamilyOnboardMintsSmallDefaultSet is the acceptance cell:
// one onboarding request writes the foreman plus -qa/-sync/-docs and NOTHING
// else, and every minted satellite carries the live family shape (canonical
// workdir, family cadence pin, cooldown floor/ceiling, family namespace, parent
// link) while staying DISARMED.
func TestSCHEDGAP1765_FamilyOnboardMintsSmallDefaultSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()
	gap1765SeedNamespaces(t, a.db, "qa", "duckbrain-sync", "docs")

	status, body := a.do(t, "POST", "/api/v1/projects", gap1765FamilyBody("fam1", nil))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %#v)", status, body)
	}

	// The template the request declared.
	fam := gap1765FamilyReport(t, body)
	if got, want := gap1765Strings(t, fam["default_roles"]), []string{"-qa", "-sync", "-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("default_roles = %v, want %v (the smaller default set)", got, want)
	}
	if got, want := gap1765Strings(t, fam["on_demand_roles"]), []string{"-pm", "-dogfood", "-perf", "-releng", "-review", "-readme"}; !reflect.DeepEqual(got, want) {
		t.Errorf("on_demand_roles = %v, want %v", got, want)
	}
	if got, want := gap1765Strings(t, fam["created"]), []string{"fam1-qa", "fam1-sync", "fam1-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("created = %v, want %v", got, want)
	}
	if got := gap1765Strings(t, fam["existing"]); len(got) != 0 {
		t.Errorf("existing = %v, want none on a fresh family", got)
	}
	if got := gap1765Strings(t, fam["errors"]); len(got) != 0 {
		t.Errorf("errors = %v, want none", got)
	}
	if fam["errors"] == nil {
		t.Error("errors must serialize as [] rather than null")
	}
	if got := gap1765Strings(t, fam["notes"]); len(got) != 0 {
		t.Errorf("notes = %v, want none once the family namespaces exist", got)
	}
	if fam["notes"] == nil {
		t.Error("notes must serialize as [] rather than null")
	}
	primary, ok := body["project"].(map[string]interface{})
	if !ok || primary["name"] != "fam1" {
		t.Fatalf("response project block = %#v, want the foreman row fam1", body["project"])
	}

	// Exactly four rows: the foreman and the three default satellites.
	if n := gap1765Count(t, a.db); n != 4 {
		t.Fatalf("projects table holds %d rows, want exactly 4 (foreman + qa + sync + docs)", n)
	}

	// The satellites carry the live family shape. Workdirs are the canonical
	// paths the SCHED-GAP-138 gate validates against, resolved under the test's
	// HERMES_HOME.
	wantSats := []struct {
		name    string
		workdir string
		ns      string
		pin     int
	}{
		{"fam1-qa", filepath.Join(home, "stand-in", "pm", "fam1"), "qa", 43200},
		{"fam1-sync", filepath.Join(home, "sync-workdirs", "fam1-sync"), "duckbrain-sync", 21600},
		{"fam1-docs", filepath.Join(home, "stand-in", "docs", "fam1"), "docs", sevenDayPin},
	}
	for _, want := range wantSats {
		got, err := database.GetProject(ctx, a.db, want.name)
		if err != nil {
			t.Errorf("read back %s: %v", want.name, err)
			continue
		}
		if got.Name != want.name {
			t.Errorf("%s: name = %q", want.name, got.Name)
		}
		if got.Parent != "fam1" {
			t.Errorf("%s: parent = %q, want the foreman fam1", want.name, got.Parent)
		}
		if got.Enabled {
			t.Errorf("%s: enabled = true, want false (create never arms a row)", want.name)
		}
		if filepath.Clean(got.Workdir) != filepath.Clean(want.workdir) {
			t.Errorf("%s: workdir = %q, want %q", want.name, got.Workdir, want.workdir)
		}
		if got.NamespaceID == nil || *got.NamespaceID != want.ns {
			t.Errorf("%s: namespace_id = %v, want %q", want.name, got.NamespaceID, want.ns)
		}
		if got.CooldownS != want.pin {
			t.Errorf("%s: cooldown_s = %d, want the family pin %d", want.name, got.CooldownS, want.pin)
		}
		if got.CooldownFloorS != want.pin {
			t.Errorf("%s: cooldown_floor_s = %d, want the family pin %d", want.name, got.CooldownFloorS, want.pin)
		}
		if got.CooldownCeilingS != want.pin*8 {
			t.Errorf("%s: cooldown_ceiling_s = %d, want the derived 8x floor %d", want.name, got.CooldownCeilingS, want.pin*8)
		}
		if got.AdmissionMode != database.AdmissionModeCooldown {
			t.Errorf("%s: admission_mode = %q, want cooldown (the admission law for a satellite)", want.name, got.AdmissionMode)
		}
		if got.RepoURL != "local:/tmp/fam1" {
			t.Errorf("%s: repo_url = %q, want the foreman's", want.name, got.RepoURL)
		}
	}

	// And NOT ONE of the six on-demand roles was minted.
	for _, suffix := range scheduler.OnDemandFamilyRoles() {
		if _, err := database.GetProject(ctx, a.db, "fam1"+suffix); err == nil {
			t.Errorf("onboarding minted the on-demand lane %q — the whole point of the smaller template is that it does not", "fam1"+suffix)
		}
	}
}

// sevenDayPin is the weekly cadence the docs/readme/perf/review families carry
// (7 days). It is written as arithmetic because ADV-R10 bans a second hardcoded
// weekly ceiling literal; the family pin values themselves are pinned against
// the host invariant script by TestSCHEDGAP138_FamilyConstantsParity.
const sevenDayPin = 7 * 24 * 60 * 60

// TestSCHEDGAP1765_OnDemandRolesStillMintable keeps the deliberate path alive:
// each of the six non-default role suffixes is still mintable, one lane at a
// time, and minting one adds EXACTLY one row (no hidden family expansion).
func TestSCHEDGAP1765_OnDemandRolesStillMintable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	// The canonical workdir root per resident family (the SCHED-GAP-138 gate's
	// accepted convention; qa/sync/docs are default-family and not in this list).
	roots := map[string]string{
		"pm":      "stand-in/pm-lane",
		"dogfood": "stand-in/dogfood",
		"releng":  "stand-in/releng-lane",
		"review":  "stand-in/review",
		"perf":    "stand-in/perf",
		"readme":  "stand-in/readme",
	}

	roles := scheduler.OnDemandFamilyRoles()
	if len(roles) != 6 {
		t.Fatalf("on-demand roles = %v, want the six non-default suffixes", roles)
	}
	for i, suffix := range roles {
		family := suffix[1:] // "-pm" -> "pm"
		root, ok := roots[family]
		if !ok {
			t.Fatalf("test has no canonical root for family %q — extend the table", family)
		}
		primary := "ondemand" + string(rune('a'+i))
		// The primary itself is NOT onboarded here: an on-demand lane is a
		// deliberate single-lane create, which is exactly what this asserts.
		lane := primary + suffix
		body := map[string]interface{}{
			"name":     lane,
			"repo_url": "local:/tmp/" + lane,
			"workdir":  filepath.Join(home, filepath.FromSlash(root), primary),
			// Armed satellites need their pacing policy — the family pin.
			"cooldown_floor_s":   satellitePinFor(family),
			"cooldown_ceiling_s": satellitePinFor(family) * 8,
		}
		before := gap1765Count(t, a.db)
		status, resp := a.do(t, "POST", "/api/v1/projects", body)
		if status != http.StatusCreated {
			t.Errorf("%s: status = %d, want 201 (body %#v)", lane, status, resp)
			continue
		}
		if _, isFamily := resp["family"]; isFamily {
			t.Errorf("%s: a plain create must return the bare project body, got a family block", lane)
		}
		if n := gap1765Count(t, a.db); n != before+1 {
			t.Errorf("%s: row count %d -> %d, want exactly one new row", lane, before, n)
		}
		if _, err := database.GetProject(ctx, a.db, lane); err != nil {
			t.Errorf("%s: not stored: %v", lane, err)
		}
	}
}

// satellitePinFor is the family cadence pin the live fleet carries, repeated
// here because the api package's table is unexported to api_test. The values
// are pinned against ops/check-fleet-invariants.py by
// TestSCHEDGAP138_FamilyConstantsParity.
func satellitePinFor(family string) int {
	switch family {
	case "qa":
		return 43200
	case "sync":
		return 21600
	case "pm", "releng":
		return 86400
	case "dogfood":
		return 259200
	default:
		return sevenDayPin
	}
}

// TestSCHEDGAP1765_FamilyMintLeavesExistingLanesUntouched proves the mint is
// additive over an already-provisioned family: a satellite that exists is
// reported in `existing` and keeps its own configuration byte-identical
// (weight, enabled state and updated_at all survive), and only the missing
// default lanes are written.
func TestSCHEDGAP1765_FamilyMintLeavesExistingLanesUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()
	gap1765SeedNamespaces(t, a.db, "qa", "duckbrain-sync", "docs")

	// A pre-existing, ENABLED, deliberately-configured sync lane (a hand-minted
	// lane from before the template change). Seeded through the DB layer
	// deliberately: the API create clamps `enabled` to false (SCHED-GAP-1729),
	// and this cell is about a row that was ALREADY armed before the mint.
	before := gap138SeedLane(t, a.db, database.Project{
		Name:             "fam2-sync",
		RepoURL:          "local:/tmp/fam2",
		Workdir:          filepath.Join(home, "sync-workdirs", "fam2-sync"),
		Weight:           33,
		Priority:         4,
		CooldownS:        21600,
		CooldownFloorS:   21600,
		CooldownCeilingS: 21600 * 8,
		Enabled:          true,
	})
	if !before.Enabled || before.Weight != 33 {
		t.Fatalf("seed shape wrong: enabled=%v weight=%d", before.Enabled, before.Weight)
	}

	status, body := a.do(t, "POST", "/api/v1/projects", gap1765FamilyBody("fam2", nil))
	if status != http.StatusCreated {
		t.Fatalf("family onboard over a partial family: status = %d, want 201 (body %#v)", status, body)
	}
	fam := gap1765FamilyReport(t, body)
	if got, want := gap1765Strings(t, fam["existing"]), []string{"fam2-sync"}; !reflect.DeepEqual(got, want) {
		t.Errorf("existing = %v, want %v", got, want)
	}
	if got, want := gap1765Strings(t, fam["created"]), []string{"fam2-qa", "fam2-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("created = %v, want %v (only the missing default lanes)", got, want)
	}

	after, err := database.GetProject(ctx, a.db, "fam2-sync")
	if err != nil {
		t.Fatalf("re-read fam2-sync: %v", err)
	}
	// Byte-for-byte untouched: every column, not just the interesting ones.
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the pre-existing lane was modified by the mint:\n before %+v\n after  %+v", before, after)
	}
	if !after.Enabled || after.Weight != 33 {
		t.Errorf("pre-existing lane lost its configuration: enabled=%v weight=%d", after.Enabled, after.Weight)
	}

	// foreman + sync (existing) + qa + docs.
	if n := gap1765Count(t, a.db); n != 4 {
		t.Errorf("projects table holds %d rows, want 4", n)
	}
}

// TestSCHEDGAP1765_PlainCreateMintsOneLane is the regression control for the
// historical behavior: without the family flag the handler writes exactly one
// row and answers with the bare Project, as every existing caller expects.
func TestSCHEDGAP1765_PlainCreateMintsOneLane(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "solo1",
		"repo_url": "local:/tmp/solo1",
		"workdir":  "/tmp/solo1",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %#v)", status, body)
	}
	if _, isFamily := body["family"]; isFamily {
		t.Error("a plain create must not answer with a family block")
	}
	if body["name"] != "solo1" {
		t.Errorf("plain create body = %#v, want the bare project", body)
	}
	if n := gap1765Count(t, a.db); n != 1 {
		t.Errorf("projects table holds %d rows, want 1 — a plain create mints no satellites", n)
	}
	if _, err := database.GetProject(ctx, a.db, "solo1"); err != nil {
		t.Errorf("read back solo1: %v", err)
	}
}

// TestSCHEDGAP1765_FamilyFlagDoesNotLeakIntoTheRow proves the flag is a
// REQUEST-SIDE control, not a stored field: it cannot be persisted into a
// project row and it does not survive as an unknown key anywhere.
func TestSCHEDGAP1765_FamilyFlagDoesNotLeakIntoTheRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()

	status, body := a.do(t, "POST", "/api/v1/projects", gap1765FamilyBody("fam3", nil))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %#v)", status, body)
	}
	primary, ok := body["project"].(map[string]interface{})
	if !ok {
		t.Fatalf("no project block in %#v", body)
	}
	if _, leaked := primary["family"]; leaked {
		t.Error("the create-only family flag leaked into the serialized project row")
	}
	got, err := database.GetProject(ctx, a.db, "fam3")
	if err != nil {
		t.Fatalf("read back fam3: %v", err)
	}
	if got.Enabled {
		t.Error("fam3: enabled = true, want false (born disarmed)")
	}
}

// TestSCHEDGAP1765_PascalCaseFamilyFlagIsHonoured guards the one trap the flag
// can fall into: the rest of the create body accepts legacy PascalCase keys
// (S06), so a caller writing "Family": true must not silently get a one-lane
// create. encoding/json matches object keys case-insensitively, which is what
// makes the probe cover both spellings — this cell pins that behaviour.
func TestSCHEDGAP1765_PascalCaseFamilyFlagIsHonoured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)

	status, body := a.do(t, "POST", "/api/v1/projects", map[string]interface{}{
		"name":     "fam5",
		"repo_url": "local:/tmp/fam5",
		"workdir":  "/tmp/fam5",
		"Family":   true,
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %#v)", status, body)
	}
	fam := gap1765FamilyReport(t, body)
	if got, want := gap1765Strings(t, fam["created"]), []string{"fam5-qa", "fam5-sync", "fam5-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("created = %v, want %v — a PascalCase Family flag must mint the default family", got, want)
	}
	if n := gap1765Count(t, a.db); n != 4 {
		t.Errorf("projects table holds %d rows, want 4", n)
	}
}

// TestSCHEDGAP1765_MissingFamilyNamespaceIsANoteNotAFailure covers a scheduler
// that does not define the family namespaces (a fresh install): the mint still
// produces all four rows, the satellites carry a NULL namespace — never a
// dangling FK reference, which the insert would reject — and the response says
// so in `notes` instead of pretending the family is complete.
func TestSCHEDGAP1765_MissingFamilyNamespaceIsANoteNotAFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HERMES_HOME", home)
	a := newAPITestServer(t)
	ctx := context.Background()
	// Deliberately NO namespace seeding.

	status, body := a.do(t, "POST", "/api/v1/projects", gap1765FamilyBody("fam4", nil))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — a missing namespace is a note, not a failure (body %#v)", status, body)
	}
	fam := gap1765FamilyReport(t, body)
	if got, want := gap1765Strings(t, fam["created"]), []string{"fam4-qa", "fam4-sync", "fam4-docs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("created = %v, want %v", got, want)
	}
	notes := gap1765Strings(t, fam["notes"])
	if len(notes) != 3 {
		t.Errorf("notes = %v, want one per namespace-less lane", notes)
	}
	for _, note := range notes {
		if !strings.Contains(note, "not registered on this scheduler") || !strings.Contains(note, "namespace_id null") {
			t.Errorf("note %q must name the missing namespace and the null fallback", note)
		}
	}
	for _, name := range []string{"fam4-qa", "fam4-sync", "fam4-docs"} {
		got, err := database.GetProject(ctx, a.db, name)
		if err != nil {
			t.Errorf("read back %s: %v", name, err)
			continue
		}
		if got.NamespaceID != nil {
			t.Errorf("%s: namespace_id = %q, want null when the namespace is undefined", name, *got.NamespaceID)
		}
	}
}
