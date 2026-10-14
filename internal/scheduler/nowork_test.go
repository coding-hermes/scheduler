package scheduler

import (
	"testing"
)

// SCHED-GAP-1655 — deliverable 3: the lane-class classification is EXPLICIT
// (one function, one table) and testable. A reporter lane keeps its timer
// cadence BY DESIGN — the no-work gate must never apply to it; a builder
// lane is the gate's whole jurisdiction. The table also pins the
// declarative override: a namespace reporter_class="reporter" pin exempts
// ANY lane name, and an unknown config value must never widen the gate
// (it reads as builder — governed).
func TestLaneClassTable(t *testing.T) {
	cases := []struct {
		name     string
		project  string
		nsConfig string
		want     string
	}{
		// Name-derived: every satellite family from the row's list.
		{"sync suffix is reporter", "9router-sync", "", laneClassReporter},
		{"pm suffix is reporter", "crier-pm", "", laneClassReporter},
		{"qa suffix is reporter", "bunker-qa", "", laneClassReporter},
		{"dogfood suffix is reporter", "scheduler-dogfood", "", laneClassReporter},
		{"releng suffix is reporter", "muster-releng", "", laneClassReporter},
		{"review suffix is reporter", "h3-review", "", laneClassReporter},
		{"docs suffix is reporter", "speclang-docs", "", laneClassReporter},
		{"readme suffix is reporter", "lore-readme", "", laneClassReporter},
		{"perf suffix is reporter", "pki-perf", "", laneClassReporter},
		// Builders: primary lanes and near-miss names.
		{"primary lane is builder", "crier", "", laneClassBuilder},
		{"primary with hyphen is builder", "coding-hermes-scheduler", "", laneClassBuilder},
		{"suffix must be at the END", "9router-sync-recovery", "", laneClassBuilder},
		{"embedded satellite word is builder", "sync-tools", "", laneClassBuilder},
		// Case/whitespace tolerance of the name derivation.
		{"case-insensitive suffix", "Bunker-QA", "", laneClassReporter},
		// Declarative override (namespace reporter_class) wins both ways.
		{"config reporter exempts a builder-named lane", "crier", "reporter", laneClassReporter},
		{"config reporter exempts an already-reporter lane", "crier-pm", "reporter", laneClassReporter},
		{"empty config falls back to the name", "crier-pm", "", laneClassReporter},
		// An unknown config value must never widen the gate: governed.
		{"unknown config reads builder", "crier-pm", "typo", laneClassBuilder},
		{"unknown config keeps a builder builder", "crier", "yes", laneClassBuilder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := laneClass(tc.project, tc.nsConfig); got != tc.want {
				t.Errorf("laneClass(%q, %q) = %q, want %q", tc.project, tc.nsConfig, got, tc.want)
			}
			if got := laneIsBuilder(tc.project, tc.nsConfig); got != (tc.want == laneClassBuilder) {
				t.Errorf("laneIsBuilder(%q, %q) = %v, disagree with class %q", tc.project, tc.nsConfig, got, tc.want)
			}
		})
	}
}

// SCHED-GAP-1655 — deliverable 1: the board-resolution + dispatchability
// logic is the tasks-mode scanner REUSED (boardOpenRows), never a second
// parser. The table pins the dispatchable-row definition on real board
// files: pending/in_progress open vocabulary, perpetual fixtures excluded,
// complete/duplicate closed, malformed rows open (never hide work).
func TestBuilderBoardHasWorkTable(t *testing.T) {
	complete := `{"id":"T-1","status":"complete"}`
	duplicate := `{"id":"T-2","status":"duplicate"}`
	pending := `{"id":"T-3","status":"pending"}`
	inProgress := `{"id":"T-4","status":"in_progress"}`
	perpetual := `{"id":"NEVER-DONE","status":"pending","perpetual":true}`

	cases := []struct {
		name string
		rows []string
		want bool
	}{
		{"empty board has no work", nil, false},
		{"only closed rows have no work", []string{complete, duplicate}, false},
		{"a pending row is work", []string{pending}, true},
		{"an in_progress row is work", []string{inProgress}, true},
		{"closed plus pending is work", []string{complete, pending}, true},
		{"perpetual fixture is NOT work", []string{perpetual}, false},
		{"perpetual plus closed is no work", []string{perpetual, complete, duplicate}, false},
		{"perpetual plus pending is work", []string{perpetual, pending}, true},
		{"malformed row counts open (never hide work)", []string{"{not json"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wd := admitWorkdirWithBoard(t, tc.rows...)
			if got := builderBoardHasWork(wd); got != tc.want {
				t.Errorf("builderBoardHasWork(rows=%d) = %v, want %v", len(tc.rows), got, tc.want)
			}
		})
	}

	t.Run("missing board is fail-open (work, timer decides)", func(t *testing.T) {
		// TempDir with no .coding-hermes/ at all: an evidence gap — the
		// fleet-wide fail-open convention applies (missing board never
		// deferred a tick before this row).
		if got := builderBoardHasWork(t.TempDir()); !got {
			t.Error("builderBoardHasWork(no board) = false, want true (fail-open)")
		}
	})
	t.Run("empty workdir is fail-open (no evidence either way)", func(t *testing.T) {
		if got := builderBoardHasWork(""); !got {
			t.Error("builderBoardHasWork(\"\") = false, want true (fail-open)")
		}
	})
}

// SCHED-GAP-1655 — the admission gate conjunction: cooldown-mode × builder
// class × empty board. Every other shape is transparent.
func TestBuilderAdmissionBlockedTable(t *testing.T) {
	emptyBoard := admitWorkdirWithBoard(t)
	withWork := admitWorkdirWithBoard(t, `{"id":"T-1","status":"pending"}`)

	cases := []struct {
		name     string
		project  string
		workdir  string
		mode     string
		nsConfig string
		want     bool
	}{
		{"cooldown builder empty board → blocked", "proj", emptyBoard, "cooldown", "", true},
		{"cooldown builder with work → proceeds", "proj", withWork, "cooldown", "", false},
		{"cooldown REPORTER empty board → proceeds (timer by design)", "proj-sync", emptyBoard, "cooldown", "", false},
		{"tasks builder empty board → gate transparent (waiver governs)", "proj", emptyBoard, "tasks", "", false},
		{"tasks reporter empty board → gate transparent", "proj-sync", emptyBoard, "tasks", "", false},
		{"ns reporter pin exempts a builder-named lane", "proj", emptyBoard, "cooldown", "reporter", false},
		{"missing board is fail-open (proceeds)", "proj", t.TempDir(), "cooldown", "", false},
		{"empty workdir is fail-open (proceeds)", "proj", "", "cooldown", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := builderAdmissionBlocked(tc.project, tc.workdir, tc.mode, tc.nsConfig)
			if got != tc.want {
				t.Errorf("builderAdmissionBlocked(%q, wd=%q, mode=%q, ns=%q) = %v, want %v",
					tc.project, tc.workdir, tc.mode, tc.nsConfig, got, tc.want)
			}
		})
	}
}

// SCHED-GAP-1655 — deliverable 2: the zero-tool signature. The envelope arm
// (no tool-call output item) and the trace arm (no observed tool event)
// together decide; either arm alone saying "acted" keeps the tick honest.
func TestZeroToolSessionTable(t *testing.T) {
	msgOnly := &Response{Output: []OutputItem{{Type: "message", Role: "assistant"}}}
	withTool := &Response{Output: []OutputItem{
		{Type: "message", Role: "assistant"},
		{Type: "function_call"},
	}}
	unknownType := &Response{Output: []OutputItem{{Type: "weird_future_shape"}}}
	noEvents := &GatewayPOSTTrace{Events: 0}
	oneEvent := &GatewayPOSTTrace{Events: 1}
	twoEvents := &GatewayPOSTTrace{Events: 2}

	cases := []struct {
		name  string
		resp  *Response
		trace *GatewayPOSTTrace
		want  bool
	}{
		{"text-only reply, silent trace → no work", msgOnly, noEvents, true},
		{"text-only reply, one event → no work", msgOnly, oneEvent, true},
		{"text-only reply, tool event seen in trace → acted", msgOnly, twoEvents, false},
		{"tool call in envelope → acted", withTool, noEvents, false},
		{"unknown item type is NOT a tool (under-count)", unknownType, noEvents, true},
		{"nil response (legacy envelope) → falls to the trace", nil, twoEvents, false},
		{"nil response, silent trace → no work", nil, oneEvent, true},
		{"nil trace → envelope alone decides", msgOnly, nil, true},
		{"nil trace with tool → acted", withTool, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := zeroToolSession(tc.resp, tc.trace); got != tc.want {
				t.Errorf("zeroToolSession(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

// SCHED-GAP-1655 — the response-side tool classifier: only real tool-call
// wire shapes count; unknown types under-count (a future shape must never
// hide a zero-tool tick behind an unrecognized name).
func TestResponseHasToolCallTable(t *testing.T) {
	cases := []struct {
		name string
		resp *Response
		want bool
	}{
		{"nil response", nil, false},
		{"empty output", &Response{}, false},
		{"function_call", &Response{Output: []OutputItem{{Type: "function_call"}}}, true},
		{"tool_use", &Response{Output: []OutputItem{{Type: "tool_use"}}}, true},
		{"custom_tool_call", &Response{Output: []OutputItem{{Type: "custom_tool_call"}}}, true},
		{"local_shell_call", &Response{Output: []OutputItem{{Type: "local_shell_call"}}}, true},
		{"apply_patch_call", &Response{Output: []OutputItem{{Type: "apply_patch_call"}}}, true},
		{"message only", &Response{Output: []OutputItem{{Type: "message"}}}, false},
		{"reasoning only", &Response{Output: []OutputItem{{Type: "reasoning"}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseHasToolCall(tc.resp); got != tc.want {
				t.Errorf("responseHasToolCall(%v) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}
