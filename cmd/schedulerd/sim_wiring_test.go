package main

// SCHED-GAP-1630 — --sim-success wiring guard (judge finding, tier2 FAIL
// verdict b7d6199b).
//
// The --sim-setup path created its SimRunner and wired only SetIdleRate:
// the --sim-success flag value was parsed and then dropped, so every
// multi-tick simulation rolled the hardcoded 0.85 default regardless of the
// operator's flag (SimRunner.SetSuccessRate had zero production call sites,
// and the judge's probe — set 0.5, run RunMultiTick — observed 0.85
// behavior).
//
// Like readme_flag_parity_test.go, this guard reads cmd/schedulerd/main.go's
// SOURCE rather than calling main(): the wiring sits inside main() behind
// the --sim-setup flag, which a test in this package cannot reach without
// booting the daemon. The checker is pure and driven twice — once against
// the real block, once against a stripped decoy — so a regex regression
// cannot turn it into a permanently-green no-op.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// simSetupWiringRe matches the flag wiring the --sim-setup block must carry:
// runner.SetSuccessRate(<arg>) with a non-blank argument.
var simSetupWiringRe = regexp.MustCompile(`runner\.SetSuccessRate\(\s*[^\s()]+\s*\)`)

// simSetupBlock extracts the body of the `if *simSetup {` block in main.go's
// source by brace counting, so a later edit that moves the block (or adds
// sibling code between the flag parse and the wiring) cannot defeat the pin.
// ok=false means the block marker is gone — the wiring claim is stale and
// the caller must fail loudly rather than pass vacuously.
func simSetupBlock(src string) (block string, ok bool) {
	const marker = "if *simSetup {"
	start := strings.Index(src, marker)
	if start < 0 {
		return "", false
	}
	depth := 0
	opened := false
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
			opened = true
		case '}':
			depth--
			if opened && depth == 0 {
				return src[start : i+1], true
			}
		}
	}
	return "", false
}

// simSetupWiringFindings is the pure checker both the live test and the decoy
// drive. It reports one finding per missing wiring element in the block.
func simSetupWiringFindings(block string) []string {
	var findings []string
	if !simSetupWiringRe.MatchString(block) {
		findings = append(findings, "the --sim-setup block never calls runner.SetSuccessRate — the --sim-success flag value is parsed and dropped, so every sim run uses the 0.85 default")
	}
	return findings
}

// TestSimSetupWiresSimSuccessRate pins the wiring: main.go's --sim-setup
// block must pass the --sim-success flag value into runner.SetSuccessRate so
// RunMultiTick installs the operator's rate on the loop's sim spawner
// (RunMultiTick's precedence — caller-set rate wins, else 0.85 default — is
// unchanged; the bug was the missing wiring, not the precedence).
func TestSimSetupWiresSimSuccessRate(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	block, ok := simSetupBlock(string(src))
	if !ok {
		t.Fatal("main.go no longer contains an `if *simSetup {` block — the --sim-setup path moved or was removed; re-anchor simSetupBlock")
	}
	for _, f := range simSetupWiringFindings(block) {
		t.Error(f)
	}
}

// TestSimSetupWiringCheckerNotVacuous is the decoy: the checker must flag a
// --sim-setup block whose SetSuccessRate wiring has been stripped, and must
// accept one that carries it — otherwise a regex regression could make the
// guard green forever while the wiring is missing again.
func TestSimSetupWiringCheckerNotVacuous(t *testing.T) {
	wired := "if *simSetup {\n\trunner.SetIdleRate(*simIdle)\n\trunner.SetSuccessRate(*simSuccess)\n}"
	if findings := simSetupWiringFindings(wired); len(findings) != 0 {
		t.Errorf("checker flagged a correctly wired block: %v", findings)
	}
	stripped := "if *simSetup {\n\trunner.SetIdleRate(*simIdle)\n}"
	findings := simSetupWiringFindings(stripped)
	if len(findings) == 0 {
		t.Fatal("checker passed a block with no SetSuccessRate wiring — the guard is vacuous")
	}
	// The decoy failure must name the actual defect.
	if !strings.Contains(findings[0], "SetSuccessRate") {
		t.Errorf("decoy finding does not name the missing wiring: %q", findings[0])
	}
}
