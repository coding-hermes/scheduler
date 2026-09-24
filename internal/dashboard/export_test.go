package dashboard

import "context"

// Test-only exports for the tape quote math (SCHED-GAP-1596): the helpers
// stay unexported in tape.go; the external test package reaches them here.

// TapeSymbolForTest exposes tapeSymbol to dashboard_test.
func TapeSymbolForTest(name string) string { return tapeSymbol(name) }

// TapeIndexMoveForTest exposes tapeIndexMove to dashboard_test.
func TapeIndexMoveForTest(curr, prior int64) (label, class string) {
	return tapeIndexMove(curr, prior)
}

// QueueEntriesForTest exposes queueEntries to dashboard_test (SCHED-GAP-1590
// sort-tension test reads the entry order the template renders from).
func (g *Generator) QueueEntriesForTest(ctx context.Context) (QueueData, error) {
	return g.queueEntries(ctx)
}
