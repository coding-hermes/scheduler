package scheduler

// SCHED-GAP-1745: CadenceGovernorFor must resolve with the SAME predicates
// the pick paths apply — admissionModeFor's chain (project override →
// namespace default → cooldown) AND the SCHED-GAP-141 board-ownership gate —
// so the cadence surface can never name a governor that did not actually
// pace the lane. These tests pin the resolution table; the wire contract
// (what a tasks_board row must NOT carry) is pinned in
// internal/api/server_cadence_governor_test.go.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// governorBoardWorkdir returns a workdir with an OWNED (owned by
// construction: the board lives inside it) empty JSONL board.
func governorBoardWorkdir(t *testing.T) string {
	t.Helper()
	wd := t.TempDir()
	path := filepath.Join(wd, ".coding-hermes", "board", "tasks.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return wd
}

func TestCadenceGovernorFor_ResolutionChain(t *testing.T) {
	nsModes := map[string]string{
		"ns-tasks": database.AdmissionModeTasks,
		"ns-cron":  database.AdmissionModeCooldown,
		"ns-empty": "",
	}
	tasks, cooldown := database.AdmissionModeTasks, database.AdmissionModeCooldown

	cases := []struct {
		name string
		mode string
		ns   string
		want string
	}{
		{"project tasks override beats a tasks namespace", tasks, "ns-tasks", database.CadenceGovernorTasksBoard},
		{"project tasks override beats a cooldown namespace", tasks, "ns-cron", database.CadenceGovernorTasksBoard},
		{"project cooldown override beats a tasks namespace", cooldown, "ns-tasks", database.CadenceGovernorCooldown},
		{"namespace tasks default when the project is silent", "", "ns-tasks", database.CadenceGovernorTasksBoard},
		{"namespace cooldown default when the project is silent", "", "ns-cron", database.CadenceGovernorCooldown},
		{"unknown namespace falls back to cooldown", "", "ns-nowhere", database.CadenceGovernorCooldown},
		{"unassigned lane falls back to cooldown", "", "", database.CadenceGovernorCooldown},
		{"empty namespace mode entry falls back to cooldown", "", "ns-empty", database.CadenceGovernorCooldown},
	}
	for _, tc := range cases {
		p := database.Project{
			Workdir:       governorBoardWorkdir(t),
			AdmissionMode: tc.mode,
		}
		if tc.ns != "" {
			ns := tc.ns
			p.NamespaceID = &ns
		}
		if got := CadenceGovernorFor(p, nsModes); got != tc.want {
			t.Errorf("%s: CadenceGovernorFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCadenceGovernorFor_BoardOwnershipGate(t *testing.T) {
	nsModes := map[string]string{"ns-tasks": database.AdmissionModeTasks}
	tasks := database.AdmissionModeTasks

	// A shared-board satellite (SCHED-GAP-141 shape): its board entry is a
	// SYMLINK into another project's workdir, so the walk resolves the board
	// outside the lane's own workdir — the waiver is refused and the pin
	// paces the lane, on the cadence surface exactly as on the pick path.
	primary := governorBoardWorkdir(t)
	satellite := t.TempDir()
	if err := os.MkdirAll(filepath.Join(satellite, ".coding-hermes"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(filepath.Join(primary, ".coding-hermes", "board"),
		filepath.Join(satellite, ".coding-hermes", "board")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	shared := database.BoardOwnershipShared
	owner := database.BoardOwnershipOwner

	cases := []struct {
		name      string
		workdir   string
		ownership string
		want      string
	}{
		{"owned board inside the workdir is board-driven", governorBoardWorkdir(t), "", database.CadenceGovernorTasksBoard},
		{"shared override paces a symlinked-board lane", satellite, shared, database.CadenceGovernorCooldown},
		{"symlinked-out board is fail-closed paced", satellite, "", database.CadenceGovernorCooldown},
		{"owner override grants a board outside the workdir", satellite, owner, database.CadenceGovernorTasksBoard},
		{"no board at all is fail-closed paced", t.TempDir(), "", database.CadenceGovernorCooldown},
	}
	for _, tc := range cases {
		p := database.Project{
			Workdir:        tc.workdir,
			AdmissionMode:  tasks,
			NamespaceID:    strPtr("ns-tasks"),
			BoardOwnership: tc.ownership,
		}
		if got := CadenceGovernorFor(p, nsModes); got != tc.want {
			t.Errorf("%s: CadenceGovernorFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func strPtr(s string) *string { return &s }
