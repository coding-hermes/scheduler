package scheduler

import (
	"strings"
	"testing"
)

// TestBuiltinPromptReadsItsOwnBoard locks the board-reading rule in place.
//
// The defect this prevents, measured: a lane refused to read its board because the
// workdir copy was behind origin/main, and reported "board read prohibited" while
// 252 rows sat dispatchable. The old base rule pointed straight at the working-copy
// file, which is exactly the path a worker or a panel loop is allowed to dirty.
//
// A prompt rule is only real if the assembled prompt carries it, so this asserts on
// buildForemanPrompt's output rather than on the constant alone.
func TestBuiltinPromptReadsItsOwnBoard(t *testing.T) {
	got := buildForemanPrompt(PackedProject{}, "test-tick")

	if !strings.Contains(got, "scripts/board_read.py") {
		t.Fatal("assembled prompt does not tell the lane to read its board with scripts/board_read.py")
	}
	if !strings.Contains(got, "DO NOT TRUST THE LOCAL COPY") {
		t.Fatal("assembled prompt lost the stale-workdir interpretation rule")
	}
	// The old instruction sent the lane at the mutable working copy. If it comes
	// back, the same refusal returns.
	if strings.Contains(got, "Read the project board: .coding-hermes/board/tasks.jsonl") {
		t.Fatal("assembled prompt still sends the lane to the mutable working-copy board")
	}
	if strings.Contains(got, "READ YOUR OWN BOARD FIRST") {
		t.Fatal("duplicate board instruction is back; the rule must appear once, at the board step")
	}
}
