package dashboard

// Test-only exports for the tape quote math (SCHED-GAP-1596): the helpers
// stay unexported in tape.go; the external test package reaches them here.

// TapeSymbolForTest exposes tapeSymbol to dashboard_test.
func TapeSymbolForTest(name string) string { return tapeSymbol(name) }

// TapeIndexMoveForTest exposes tapeIndexMove to dashboard_test.
func TapeIndexMoveForTest(curr, prior int64) (label, class string) {
	return tapeIndexMove(curr, prior)
}
