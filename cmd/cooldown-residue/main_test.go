package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/cooldownaudit"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1670 — the detector's contract as a scheduled surface: exit 0 when
// the fleet is clean, exit 1 when residue exists, exit 2 when it could not look.
// The distinction is the whole point of scheduling it: a monitor that conflates
// "clean" with "could not read" trains its operator to ignore it.

type fixture struct {
	dbPath   string
	tomlPath string
}

func newFixture(t *testing.T, lanes []struct {
	name      string
	live, pin int
	enabled   bool
}, toml string) fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "scheduler.db")
	db, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	for _, lane := range lanes {
		p := &database.Project{
			Name: lane.name, RepoURL: "local:/tmp/" + lane.name, Workdir: "/tmp/" + lane.name,
			Weight: 10, Priority: 5, CooldownS: lane.live, DecayRate: 1,
			Model: "m", Provider: "p", Enabled: lane.enabled,
		}
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", lane.name, err)
		}
		if lane.pin > 0 {
			// SetCooldownPin snaps the live value up, so the residue shape is
			// then produced the way it happens in the field: a later plain
			// cooldown write (the wake-PUT path).
			if _, err := database.SetCooldownPin(ctx, db, lane.name, lane.pin, database.CooldownPinImportBy); err != nil {
				t.Fatalf("SetCooldownPin(%s): %v", lane.name, err)
			}
			live := lane.live
			if err := database.UpdateProject(ctx, db, lane.name, database.ProjectUpdates{CooldownS: &live}); err != nil {
				t.Fatalf("UpdateProject(%s): %v", lane.name, err)
			}
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	tomlPath := filepath.Join(dir, "fleet.toml")
	if toml == "" {
		tomlPath = filepath.Join(dir, "absent.toml") // no seed on disk
	} else if err := os.WriteFile(tomlPath, []byte(toml), 0o644); err != nil {
		t.Fatalf("write fleet.toml: %v", err)
	}
	return fixture{dbPath: dbPath, tomlPath: tomlPath}
}

func TestRun_ResidueExitsOneAndNamesTheLanes(t *testing.T) {
	fx := newFixture(t, []struct {
		name      string
		live, pin int
		enabled   bool
	}{
		{name: "warpfs", live: 900, pin: 21600, enabled: true},
		{name: "h3", live: 43200, pin: 43200, enabled: true},
	}, "")
	var stdout, stderr bytes.Buffer

	code := run([]string{"-db", fx.dbPath, "-fleet-toml", fx.tomlPath, "-json"}, &stdout, &stderr)
	if code != exitResidue {
		t.Fatalf("exit code = %d, want %d (residue present); stderr=%s stdout=%s", code, exitResidue, stderr.String(), stdout.String())
	}
	var report struct {
		Scanned int  `json:"enabled_lanes_scanned"`
		Clean   bool `json:"clean"`
		Residue []struct {
			Name      string `json:"name"`
			LiveS     int    `json:"live_s"`
			PinS      int    `json:"pin_s"`
			DeltaS    int    `json:"delta_s"`
			PinSource string `json:"pin_source"`
		} `json:"residue"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode JSON report: %v (raw: %s)", err, stdout.String())
	}
	if report.Clean || report.Scanned != 2 || len(report.Residue) != 1 {
		t.Fatalf("report = %+v, want clean=false scanned=2 and exactly one residue lane", report)
	}
	if got := report.Residue[0]; got.Name != "warpfs" || got.LiveS != 900 || got.PinS != 21600 || got.DeltaS != 20700 {
		t.Errorf("residue entry = %+v, want warpfs live 900 pin 21600 delta 20700", got)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("stderr = %q, want the missing-seed warning (db pins only)", stderr.String())
	}
}

func TestRun_TextModePrintsAlertLine(t *testing.T) {
	fx := newFixture(t, []struct {
		name      string
		live, pin int
		enabled   bool
	}{
		{name: "python-audit-lint", live: 900, pin: 86400, enabled: true},
	}, "")
	var stdout, stderr bytes.Buffer

	code := run([]string{"-db", fx.dbPath, "-fleet-toml", fx.tomlPath}, &stdout, &stderr)
	if code != exitResidue {
		t.Fatalf("exit code = %d, want %d", code, exitResidue)
	}
	out := stdout.String()
	for _, want := range []string{"ALERT: 1 enabled lane(s)", "python-audit-lint", "scanned 1 enabled lane(s)", "no state was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q; got:\n%s", want, out)
		}
	}
}

func TestRun_CleanExitsZero(t *testing.T) {
	fx := newFixture(t, []struct {
		name      string
		live, pin int
		enabled   bool
	}{
		{name: "h3", live: 43200, pin: 43200, enabled: true},
		{name: "paused-residue", live: 900, pin: 86400, enabled: false},
	}, "")
	var stdout, stderr bytes.Buffer

	code := run([]string{"-db", fx.dbPath, "-fleet-toml", fx.tomlPath}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d; stdout=%s", code, exitClean, stdout.String())
	}
	if !strings.Contains(stdout.String(), "OK: no enabled lane sits below its own cooldown pin") {
		t.Errorf("report = %q, want the OK line", stdout.String())
	}

	// -quiet on a clean fleet must print nothing at all (a silent cron), while
	// still reporting through the exit code.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-db", fx.dbPath, "-fleet-toml", fx.tomlPath, "-quiet"}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("quiet exit code = %d, want %d", code, exitClean)
	}
	if stdout.Len() != 0 {
		t.Errorf("quiet clean run printed %q, want silence", stdout.String())
	}
}

func TestRun_SeedPinAloneFindsResidue(t *testing.T) {
	// A lane with NO DB pin whose fleet.toml entry declares a weekly cadence:
	// the import never happened (or the row predates it), and the file is the
	// only record of the operator's intent. The detector must still fire.
	toml := `
[[projects]]
name = "bunker-perf"
workdir = "/tmp/bunker-perf"
cooldown_s = 604800
enabled = true
`
	fx := newFixture(t, []struct {
		name      string
		live, pin int
		enabled   bool
	}{
		{name: "bunker-perf", live: 86400, enabled: true},
	}, toml)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-db", fx.dbPath, "-fleet-toml", fx.tomlPath, "-json"}, &stdout, &stderr)
	if code != exitResidue {
		t.Fatalf("exit code = %d, want %d; stderr=%s", code, exitResidue, stderr.String())
	}
	if !strings.Contains(stdout.String(), cooldownaudit.PinSourceFleetToml) {
		t.Errorf("report = %q, want the pin_source %q", stdout.String(), cooldownaudit.PinSourceFleetToml)
	}
}

func TestRun_UnreadableStateIsExitTwo(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-db", filepath.Join(t.TempDir(), "absent.db")}, &stdout, &stderr)
		if code != exitError {
			t.Fatalf("exit code = %d, want %d", code, exitError)
		}
		if !strings.Contains(stderr.String(), "not accessible") {
			t.Errorf("stderr = %q, want the accessibility error", stderr.String())
		}
	})

	t.Run("empty database refuses to report clean", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "scheduler.db")
		db, err := database.InitDB(dbPath)
		if err != nil {
			t.Fatalf("InitDB: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"-db", dbPath, "-fleet-toml", filepath.Join(t.TempDir(), "absent.toml"), "-quiet"}, &stdout, &stderr)
		if code != exitError {
			t.Fatalf("exit code = %d, want %d (0 lanes read must never read as clean)", code, exitError)
		}
		if !strings.Contains(stderr.String(), "refusing to report clean") {
			t.Errorf("stderr = %q, want the refusal line", stderr.String())
		}
	})
}

func TestRun_UsageErrorsAreExitTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-not-a-flag"}, &stdout, &stderr); code != exitError {
		t.Errorf("exit code for an unknown flag = %d, want %d", code, exitError)
	}
}

func TestRun_VersionExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != exitClean {
		t.Fatalf("exit code = %d, want %d", code, exitClean)
	}
	if !strings.Contains(stdout.String(), "cooldown-residue") {
		t.Errorf("version output = %q, want it to name the command", stdout.String())
	}
}
