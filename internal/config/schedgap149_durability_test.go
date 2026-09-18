package config

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-149 file-driven durability tests: the fleet.toml RE-PIN PARITY
// contract for the 2026-09-17/18 config work.
//
// The sibling tests in this package (schedgap149_test.go, schedgap141_test.go)
// build a *FleetConfig in-process, so they prove the loader's re-pin blocks
// work — but not that a real fleet.toml FILE feeds them. That is the half the
// measured failures actually took: the operator flipped the qa/pm/dogfood/
// sync namespaces to admission_mode="cooldown" in BOTH stores, and the fleet
// shape (global --max-concurrent 10, foremen namespace 8, satellites 1) lives
// only in the file + DB pair. If the file half is lost — a stale copy, a
// renamed key, an operator edit that never round-tripped — nothing fails: the
// loader parses an empty value, its re-pin block is a keyless no-op for that
// key, and the DB silently keeps whatever it had. That is the same "file
// reverts to template" class that already bit the 9router repo.
//
// These tests therefore drive the real path end to end:
//
//	fixture fleet.toml on disk -> LoadFleetConfig -> ApplyFleetConfig over a
//	deliberately DRIFTED DB -> assert the file's values won.
//
// Every drift row runs on a FRESH database so one failing row cannot cascade,
// and every row is RED-provable on its own (delete the matching re-pin block in
// loader.go and that row, and only that row, goes red).

// schedgap149FixtureToml is the fixture fleet.toml, carrying the shape named in
// the row brief: a foremen namespace pinned to admission_mode="cooldown" +
// max_concurrent=8 + load_gate="off", a satellite namespace entry that carries
// NO admission_mode key, and a project-level admission_mode override.
const schedgap149FixtureToml = `[[namespaces]]
id = "foremen"
weight = 70
reserved = 10
hard_cap = 0
enabled = true
max_concurrent = 8
admission_mode = "cooldown"
load_gate = "off"

[[namespaces]]
id = "satellite"
weight = 30
reserved = 1
hard_cap = 100
enabled = true

[[projects]]
name = "lane-foremen"
repo_url = "local://lane-foremen"
workdir = "/tmp/lane-foremen"
namespace_id = "foremen"
enabled = true

[[projects]]
name = "lane-satellite"
repo_url = "local://lane-satellite"
workdir = "/tmp/lane-satellite"
namespace_id = "satellite"
enabled = true
admission_mode = "tasks"
`

// loadSchedgap149Fixture writes the fixture to a temp dir and loads it the way
// the daemon does (--config path -> LoadFleetConfig).
func loadSchedgap149Fixture(t *testing.T) *FleetConfig {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(path, []byte(schedgap149FixtureToml), 0o644); err != nil {
		t.Fatalf("write fixture fleet.toml: %v", err)
	}
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig(fixture): %v", err)
	}
	return cfg
}

// TestSCHEDGAP149_FileKeysParseIntoThePinnedFields pins the TOML KEY NAMES
// themselves. A renamed or mistyped key is the silent half of this failure
// class: the file looks right, decodes to the zero value, and the re-pin block
// (which skips empty values by design) becomes a no-op for that key. If this
// test goes red, every drift row in this file is vacuous.
func TestSCHEDGAP149_FileKeysParseIntoThePinnedFields(t *testing.T) {
	cfg := loadSchedgap149Fixture(t)

	ns := findNamespace(cfg, "foremen")
	if ns == nil {
		t.Fatal("T-149-F0 FAIL: namespace foremen missing from the parsed fixture")
	}
	if ns.MaxConcurrent != 8 {
		t.Errorf("T-149-F0a FAIL: [[namespaces]] max_concurrent did not decode: got %d want 8", ns.MaxConcurrent)
	}
	if ns.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-F0b FAIL: [[namespaces]] admission_mode did not decode: got %q want %q", ns.AdmissionMode, database.AdmissionModeCooldown)
	}
	if ns.LoadGate != "off" {
		t.Errorf("T-149-F0c FAIL: [[namespaces]] load_gate did not decode: got %q want %q", ns.LoadGate, "off")
	}

	// satellite carries no keys at all — the parser must leave them zero, and
	// the CREATE path is what supplies the documented default (next test).
	sat := findNamespace(cfg, "satellite")
	if sat == nil {
		t.Fatal("T-149-F0 FAIL: namespace satellite missing from the parsed fixture")
	}
	if sat.AdmissionMode != "" {
		t.Errorf("T-149-F0d FAIL: satellite carries no admission_mode key but decoded %q", sat.AdmissionMode)
	}
	if sat.MaxConcurrent != 0 {
		t.Errorf("T-149-F0e FAIL: satellite carries no max_concurrent key but decoded %d", sat.MaxConcurrent)
	}

	p := findProject(cfg, "lane-satellite")
	if p == nil {
		t.Fatal("T-149-F0 FAIL: project lane-satellite missing from the parsed fixture")
	}
	if p.AdmissionMode != database.AdmissionModeTasks {
		t.Errorf("T-149-F0f FAIL: [[projects]] admission_mode did not decode: got %q want %q", p.AdmissionMode, database.AdmissionModeTasks)
	}
	if findProject(cfg, "lane-foremen").AdmissionMode != "" {
		t.Errorf("T-149-F0g FAIL: lane-foremen carries no admission_mode key but decoded %q", findProject(cfg, "lane-foremen").AdmissionMode)
	}
}

// TestSCHEDGAP149_NamespaceModeCanNeverBeEmpty pins the schema fact that shapes
// the whole precedence law on the namespace rung: namespaces.admission_mode is
// NOT NULL DEFAULT 'cooldown' CHECK IN ('cooldown','tasks'), so there is NO such
// thing as a namespace with an empty admission_mode.
//
// Two consequences the row brief's "no namespace default" scenario runs into:
//
//  1. A [[namespaces]] entry with no admission_mode key is materialized as
//     "cooldown" at create time (namespaceFromDef -> nsAdmissionMode), not "".
//  2. A namespace can never be returned with "" — CreateNamespace normalizes
//     "" to the default before it reaches the column, and an operator's hand
//     INSERT of ” would trip the CHECK.
//
// The empty-string rung therefore exists only at the PROJECT level ("", =
// inherit) and for a namespace that is absent from the map/DB entirely, which
// is exactly what admissionModeFor treats as the cooldown default (pinned in
// internal/scheduler/schedgap149_precedence_test.go).
func TestSCHEDGAP149_NamespaceModeCanNeverBeEmpty(t *testing.T) {
	cfg := loadSchedgap149Fixture(t)
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// (1) The keyless file entry materializes the create-time default.
	sat, err := database.GetNamespace(ctx, db, "satellite")
	if err != nil {
		t.Fatalf("GetNamespace(satellite): %v", err)
	}
	if sat.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-K1 FAIL: a keyless namespace entry materialized admission_mode %q, want %q (the create-time default)",
			sat.AdmissionMode, database.AdmissionModeCooldown)
	}

	// (2) A namespace created directly with "" comes back as the default —
	// the normalization is on the write path, not only in the loader.
	hand := &database.Namespace{ID: "hand-made", Weight: 10, Reserved: 1, HardCap: 100, Enabled: true, AdmissionMode: ""}
	if err := database.CreateNamespace(ctx, db, hand); err != nil {
		t.Fatalf("CreateNamespace(default): %v", err)
	}
	got, err := database.GetNamespace(ctx, db, "hand-made")
	if err != nil {
		t.Fatalf("GetNamespace(hand-made): %v", err)
	}
	if got.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-K2 FAIL: CreateNamespace stored admission_mode %q for an empty input, want %q — a namespace can never be defaultless",
			got.AdmissionMode, database.AdmissionModeCooldown)
	}
	// The column itself refuses an empty value, so no future writer can create
	// the unreachable state this test documents as impossible.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO namespaces (id, weight, reserved, hard_cap, max_concurrent, enabled, admission_mode)
		 VALUES ('illegal', 10, 1, 100, 1, 1, '')`); err == nil {
		t.Errorf("T-149-K3 FAIL: the namespaces schema accepted admission_mode='' — the empty namespace rung became reachable")
	}
}

// TestSCHEDGAP149_FilePinsRepinOverDBDrift is the parity contract itself: for
// each pinned key, drift the DB away from the file, re-run the loader over the
// SAME file-loaded config, and assert the file's value is restored.
func TestSCHEDGAP149_FilePinsRepinOverDBDrift(t *testing.T) {
	// The fixture is parsed once: the file is the input under test, and
	// parsing it per row would not change any assertion.
	cfg := loadSchedgap149Fixture(t)

	cases := []struct {
		name  string
		drift func(t *testing.T, ctx context.Context, db *sql.DB)
		check func(t *testing.T, ctx context.Context, db *sql.DB)
	}{
		{
			name: "namespace admission_mode cooldown beats a tasks drift",
			drift: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				if err := database.UpdateNamespace(ctx, db, "foremen", database.NamespacePatch{
					AdmissionMode: strPtr(database.AdmissionModeTasks),
				}); err != nil {
					t.Fatalf("drift admission_mode: %v", err)
				}
			},
			check: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				ns, err := database.GetNamespace(ctx, db, "foremen")
				if err != nil {
					t.Fatalf("GetNamespace: %v", err)
				}
				if ns.AdmissionMode != database.AdmissionModeCooldown {
					t.Errorf("T-149-F1 FAIL: namespace admission_mode drift survived the restart: got %q want %q", ns.AdmissionMode, database.AdmissionModeCooldown)
				}
			},
		},
		{
			name: "namespace max_concurrent 8 beats a 4 drift",
			drift: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				if err := database.UpdateNamespace(ctx, db, "foremen", database.NamespacePatch{
					MaxConcurrent: intPtr(4),
				}); err != nil {
					t.Fatalf("drift max_concurrent: %v", err)
				}
			},
			check: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				ns, err := database.GetNamespace(ctx, db, "foremen")
				if err != nil {
					t.Fatalf("GetNamespace: %v", err)
				}
				if ns.MaxConcurrent != 8 {
					t.Errorf("T-149-F2 FAIL: namespace max_concurrent drift survived the restart: got %d want 8", ns.MaxConcurrent)
				}
			},
		},
		{
			name: "namespace load_gate off beats a drift to empty",
			drift: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				if err := database.UpdateNamespace(ctx, db, "foremen", database.NamespacePatch{
					LoadGate: strPtr(""),
				}); err != nil {
					t.Fatalf("drift load_gate: %v", err)
				}
			},
			check: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				ns, err := database.GetNamespace(ctx, db, "foremen")
				if err != nil {
					t.Fatalf("GetNamespace: %v", err)
				}
				if ns.LoadGate != "off" {
					t.Errorf("T-149-F3 FAIL: namespace load_gate drift survived the restart: got %q want %q", ns.LoadGate, "off")
				}
			},
		},
		{
			// The brief's project-override scenario, in the only shape the
			// schema allows: the project override is 'tasks' while its
			// namespace default says the OPPOSITE ('cooldown'). The namespace
			// cannot be neutral (see the schema test above), so the project
			// rung has to beat an actively disagreeing namespace for the
			// override to be durable at all.
			name: "project admission_mode tasks beats a cooldown drift over an opposite namespace default",
			drift: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				if err := database.UpdateProject(ctx, db, "lane-satellite", database.ProjectUpdates{
					AdmissionMode: strPtr(database.AdmissionModeCooldown),
				}); err != nil {
					t.Fatalf("drift project admission_mode: %v", err)
				}
			},
			check: func(t *testing.T, ctx context.Context, db *sql.DB) {
				t.Helper()
				p, err := database.GetProject(ctx, db, "lane-satellite")
				if err != nil {
					t.Fatalf("GetProject: %v", err)
				}
				if p.AdmissionMode != database.AdmissionModeTasks {
					t.Errorf("T-149-F4 FAIL: project admission_mode drift survived the restart: got %q want %q", p.AdmissionMode, database.AdmissionModeTasks)
				}
				// The namespace default must be untouched by the project pin:
				// if the loader had widened the project override to the
				// namespace, the precedence rungs would have collapsed.
				ns, err := database.GetNamespace(ctx, db, "satellite")
				if err != nil {
					t.Fatalf("GetNamespace: %v", err)
				}
				if ns.AdmissionMode != database.AdmissionModeCooldown {
					t.Errorf("T-149-F4b FAIL: the namespace default was rewritten by a project pin: got %q want %q", ns.AdmissionMode, database.AdmissionModeCooldown)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.InitDB(":memory:")
			if err != nil {
				t.Fatalf("InitDB: %v", err)
			}
			defer db.Close()
			ctx := context.Background()

			// Boot 1: create from the file.
			if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
				t.Fatalf("ApplyFleetConfig (boot 1): %v", err)
			}
			// Drift the DB away from the file.
			tc.drift(t, ctx, db)
			// Boot 2 (restart): the loader re-pins from the SAME file.
			if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
				t.Fatalf("ApplyFleetConfig (boot 2): %v", err)
			}
			tc.check(t, ctx, db)
		})
	}
}

// TestSCHEDGAP149_PrecedenceInputsAreProjectNamespaceDefault pins the three
// rungs the resolution law reads, as the LOADER leaves them in the DB. The
// resolver itself (project > namespace > cooldown default) is admissionModeFor,
// which lives in internal/scheduler — see
// TestSCHEDGAP149_AdmissionModePrecedence and
// TestSCHEDGAP149_AdmissionModePrecedenceMatchesDBResolver there for the
// table-driven assertions over exactly these three states.
func TestSCHEDGAP149_PrecedenceInputsAreProjectNamespaceDefault(t *testing.T) {
	cfg := loadSchedgap149Fixture(t)
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := ApplyFleetConfig(ctx, db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}

	// Rung 1 (project override): lane-satellite carries its own mode while its
	// namespace default disagrees.
	p, err := database.GetProject(ctx, db, "lane-satellite")
	if err != nil {
		t.Fatalf("GetProject(lane-satellite): %v", err)
	}
	if p.AdmissionMode != database.AdmissionModeTasks {
		t.Errorf("T-149-P1 FAIL: project override rung is not populated: got %q want %q", p.AdmissionMode, database.AdmissionModeTasks)
	}
	sat, err := database.GetNamespace(ctx, db, "satellite")
	if err != nil {
		t.Fatalf("GetNamespace(satellite): %v", err)
	}
	if sat.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-P2 FAIL: the namespace rung under the override is not the disagreeing default: got %q want %q", sat.AdmissionMode, database.AdmissionModeCooldown)
	}

	// Rung 2 (namespace default): lane-foremen carries no override, so its
	// resolution must come from the namespace.
	np, err := database.GetProject(ctx, db, "lane-foremen")
	if err != nil {
		t.Fatalf("GetProject(lane-foremen): %v", err)
	}
	if np.AdmissionMode != "" {
		t.Errorf("T-149-P3 FAIL: lane-foremen should carry no project override, got %q", np.AdmissionMode)
	}
	foremen, err := database.GetNamespace(ctx, db, "foremen")
	if err != nil {
		t.Fatalf("GetNamespace(foremen): %v", err)
	}
	if foremen.AdmissionMode != database.AdmissionModeCooldown {
		t.Errorf("T-149-P4 FAIL: namespace default rung is not populated: got %q want %q", foremen.AdmissionMode, database.AdmissionModeCooldown)
	}

	// Rung 3 (the cooldown default) has no row to read: the schema forbids an
	// empty namespace mode, so the fall-through is only reachable for an
	// ABSENT namespace. It is asserted in the scheduler package against the
	// resolver directly; here it is the narrowing this test documents.
	var empties int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM namespaces WHERE admission_mode = ''`).Scan(&empties); err != nil {
		t.Fatalf("count defaultless namespaces: %v", err)
	}
	if empties != 0 {
		t.Errorf("T-149-P5 FAIL: found %d namespaces with an empty admission_mode — the fall-through rung became reachable", empties)
	}
}
