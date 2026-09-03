package blocks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedNow is a deterministic deploy time (UTC) so generated ids are stable.
func fixedNow() time.Time {
	return time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
}

func TestDeployDryRunWritesNothing(t *testing.T) {
	wd1 := t.TempDir() // valid workdir, but NO board — dry run must report
	// error without creating the board tree.
	group := Group{Name: "grp", Projects: []string{"alpha", "ghost"}}
	tpl := sampleTemplate()
	res := Deploy(DeployRequest{
		Group:    group,
		Template: tpl,
		Projects: []ProjectTarget{
			{Name: "alpha", Workdir: wd1},
			// ghost deliberately absent from Projects → unknown project error
		},
		DryRun: true,
		Now:    fixedNow(),
	})
	if res.DryRun != true {
		t.Fatalf("DryRun = %v", res.DryRun)
	}
	if len(res.Projects) != 2 {
		t.Fatalf("outcomes = %d, want 2", len(res.Projects))
	}
	byName := map[string]ProjectOutcome{}
	for _, o := range res.Projects {
		byName[o.Project] = o
	}
	if byName["alpha"].Status != "error" {
		t.Errorf("alpha status = %q, want error (no board)", byName["alpha"].Status)
	}
	if byName["ghost"].Status != "error" {
		t.Errorf("ghost status = %q, want error (unknown project)", byName["ghost"].Status)
	}
	// Dry run must not have created the board directory tree.
	if _, err := os.Stat(filepath.Join(wd1, ".coding-hermes")); !os.IsNotExist(err) {
		t.Fatalf("dry run created .coding-hermes tree: %v", err)
	}
	if res.Summary.Errors != 2 || res.Summary.Appended != 0 {
		t.Errorf("summary = %+v, want 2 errors", res.Summary)
	}
}

func TestDeployBatchContinuesPastErrors(t *testing.T) {
	goodWD, goodBoard := makeBoard(t, "")
	noBoardWD := t.TempDir()
	res := Deploy(DeployRequest{
		Group:    Group{Name: "grp", Projects: []string{"good", "noboard", "missing"}},
		Template: sampleTemplate(),
		Projects: []ProjectTarget{
			{Name: "good", Workdir: goodWD},
			{Name: "noboard", Workdir: noBoardWD},
			// "missing" not in targets — DB resolution failure
		},
		Now: fixedNow(),
	})
	if len(res.Projects) != 3 {
		t.Fatalf("outcomes = %d, want 3 (batch never aborts)", len(res.Projects))
	}
	byName := map[string]ProjectOutcome{}
	for _, o := range res.Projects {
		byName[o.Project] = o
	}
	if byName["good"].Status != "appended" || len(byName["good"].TaskIDs) != 2 {
		t.Fatalf("good outcome = %+v, want appended with 2 ids", byName["good"])
	}
	if byName["good"].TaskIDs[0] != "QUALITY-SWEEP-20260903-good-01" {
		t.Errorf("first id = %q", byName["good"].TaskIDs[0])
	}
	if byName["noboard"].Status != "error" {
		t.Errorf("noboard status = %q, want error", byName["noboard"].Status)
	}
	if byName["missing"].Status != "error" {
		t.Errorf("missing status = %q, want error", byName["missing"].Status)
	}
	if res.Summary.Appended != 1 || res.Summary.Errors != 2 {
		t.Errorf("summary = %+v, want 1 appended 2 errors", res.Summary)
	}
	// The good board really got 2 parseable pending rows.
	lines := readBoardLines(t, goodBoard)
	if len(lines) != 2 {
		t.Fatalf("good board lines = %d, want 2", len(lines))
	}
}

func TestDeployIdempotentRedeploy(t *testing.T) {
	wd, board := makeBoard(t, "")
	group := Group{Name: "grp", Projects: []string{"alpha"}}
	tpl := sampleTemplate()
	req := DeployRequest{
		Group:    group,
		Template: tpl,
		Projects: []ProjectTarget{{Name: "alpha", Workdir: wd}},
		Now:      fixedNow(),
	}
	first := Deploy(req)
	if first.Projects[0].Status != "appended" {
		t.Fatalf("first deploy = %+v", first.Projects[0])
	}
	lines := readBoardLines(t, board)
	if len(lines) != 2 {
		t.Fatalf("board lines after first deploy = %d, want 2", len(lines))
	}
	// Dry-run plan now says would_skip.
	dry := Deploy(DeployRequest{
		Group: group, Template: tpl,
		Projects: []ProjectTarget{{Name: "alpha", Workdir: wd}},
		DryRun:   true,
		Now:      fixedNow(),
	})
	if dry.Projects[0].Status != "would_skip" {
		t.Errorf("dry redeploy status = %q, want would_skip (%s)", dry.Projects[0].Status, dry.Projects[0].Reason)
	}
	// Live redeploy skips — nothing appended twice.
	second := Deploy(req)
	if second.Projects[0].Status != "skipped" {
		t.Fatalf("second deploy = %+v, want skipped", second.Projects[0])
	}
	if got := len(readBoardLines(t, board)); got != 2 {
		t.Fatalf("board lines after redeploy = %d, want 2 (idempotent)", got)
	}
}

func TestDeployTitleSubstitutionAndRowShape(t *testing.T) {
	wd, board := makeBoard(t, "")
	tpl := Template{
		Name: "SITE-RELIABILITY",
		Tasks: []TemplateTask{
			{Title: "Alert audit on {PROJECT}", Detail: "Check alert routing for {PROJECT} on {DATE}", Labels: []string{"sre", "alerts"}},
		},
	}
	res := Deploy(DeployRequest{
		Group:    Group{Name: "grp", Projects: []string{"edge"}},
		Template: tpl,
		Projects: []ProjectTarget{{Name: "edge", Workdir: wd}},
		Now:      fixedNow(),
	})
	if res.Projects[0].Status != "appended" {
		t.Fatalf("deploy = %+v", res.Projects[0])
	}
	lines := readBoardLines(t, board)
	if len(lines) != 1 {
		t.Fatalf("board lines = %d", len(lines))
	}
	row := parseBoardRowJSON(t, lines[0])
	if row["id"] != "SITE-RELIABILITY-20260903-edge-01" {
		t.Errorf("row id = %v", row["id"])
	}
	if row["title"] != "Alert audit on edge" {
		t.Errorf("row title = %v (expected {PROJECT} substitution)", row["title"])
	}
	if row["status"] != "pending" || row["worker_status"] != "pending" {
		t.Errorf("row statuses = %v / %v", row["status"], row["worker_status"])
	}
	reasoning, ok := row["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("reasoning = %T, want object carrying task detail", row["reasoning"])
	}
	note, _ := reasoning["note"].(string)
	if note != "Check alert routing for edge on 20260903" {
		t.Errorf("reasoning.note = %q", note)
	}
	// 31-key canonical shape: every key present (nulls allowed, missing not).
	if len(row) != 31 {
		t.Errorf("row has %d keys, want the canonical 31", len(row))
	}
}
