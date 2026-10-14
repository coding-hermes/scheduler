package main

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/config"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1696 criterion 3. One tasks namespace; a foreman f1 (owner of
// f1-qa) and its satellite f1-qa — the satellite carries lane-level tasks, a
// contradiction the boot pass must correct.
const gap1696BootToml = `[[namespaces]]
id = "coding-hermes"
weight = 100
admission_mode = "tasks"

[[projects]]
name = "f1"
repo_url = "https://example.com/f1"
workdir = "/tmp/f1"
weight = 1
priority = 1
cooldown_s = 900
decay_rate = 1.0
model = "test"
provider = "test"
namespace_id = "coding-hermes"
admission_mode = "tasks"

[[projects]]
name = "f1-qa"
repo_url = "https://example.com/f1-qa"
workdir = "/tmp/f1-qa"
weight = 1
priority = 1
cooldown_s = 21600
decay_rate = 1.0
model = "test"
provider = "test"
namespace_id = "coding-hermes"
admission_mode = "tasks"
`

func seedBoot1696(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	p := filepath.Join(dir, "fleet.toml")
	if err := os.WriteFile(p, []byte(gap1696BootToml), 0o644); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	cfg, err := config.LoadFleetConfig(p)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if err := config.ApplyFleetConfig(context.Background(), db, cfg); err != nil {
		t.Fatalf("ApplyFleetConfig: %v", err)
	}
	return db
}

func TestApplyAdmissionLaw_CorrectsAndLogs(t *testing.T) {
	db := seedBoot1696(t)
	ctx := context.Background()

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	applyAdmissionLaw(ctx, db)
	log.SetOutput(old)

	got := buf.String()
	if !strings.Contains(got, "ADMISSION-LAW") {
		t.Fatalf("expected an ADMISSION-LAW correction line, got: %q", got)
	}
	if !strings.Contains(got, "f1-qa") {
		t.Fatalf("ADMISSION-LAW line does not name the lane: %q", got)
	}

	p, err := database.GetProject(ctx, db, "f1-qa")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if p.AdmissionMode != database.AdmissionModeCooldown {
		t.Fatalf("f1-qa not corrected: admission_mode=%q, want cooldown", p.AdmissionMode)
	}

	// The foreman is already correct and must be untouched.
	f, _ := database.GetProject(ctx, db, "f1")
	if f.AdmissionMode != database.AdmissionModeTasks {
		t.Fatalf("foreman f1 changed: %q", f.AdmissionMode)
	}

	// A clean re-run logs ZERO ADMISSION-LAW lines.
	buf.Reset()
	log.SetOutput(&buf)
	applyAdmissionLaw(ctx, db)
	log.SetOutput(old)
	if strings.Contains(buf.String(), "ADMISSION-LAW") {
		t.Fatalf("clean re-run logged a correction: %q", buf.String())
	}
}
