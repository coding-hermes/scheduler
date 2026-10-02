package scheduler

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// SCHED-GAP-1687 — the gateway-failure log must name the REAL outcome, not a
// phantom exec fallback.
//
// The defect, measured on the live daemon 2026-09-30: spawn.go logged
// "GATEWAY FAIL: … — falling back to exec.Command" BEFORE the branchy
// classification below it, so the line fired on every gateway failure even
// when exec fallback was disabled (the default: --no-exec-fallback DEFAULT
// true) — and even when a fallback-enabled run then took an early return that
// never execs (the SCHED-GAP-117 stalled-turn path). 15 live occurrences,
// every one followed by FAIL, 9 by DEFERRED, not one exec. Diagnostics that
// assert a state that does not exist are worse than no diagnostics.
//
// The fix moves the wording to the truth:
//   - the "falling back to exec.Command" text now logs only at the site that
//     actually calls exec.Command;
//   - the no-fallback drop logs "exec fallback disabled, dropping tick" (the
//     line that was already there — now the FIRST word on the failure, not a
//     correction of a phantom claim two lines earlier);
//   - the classified helpers keep their own outcome lines (GATEWAY STALLED /
//     TIMEOUT / DEFERRED / GATEWAY KEY REJECTED).
//
// Acceptance criterion 2 drives a gateway failure with each flag value and
// asserts the no-fallback run does NOT contain "falling back to exec" and
// DOES name its real outcome. No t.Parallel anywhere in this package, so the
// global log.SetOutput capture is safe under -p 1 (same shape as the
// schedGap080 helpers).

// sgap1687CaptureLog redirects the process logger into a buffer for the
// duration of the test and restores it on cleanup.
func sgap1687CaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

// sgap1687FailingGatewayServer serves HTTP 500 on every request — the
// *GatewayStatusError shape. It is transient (so the SCHED-GAP-080 bounded
// retry exhausts its 3 attempts, ~3.5s worst case) but NOT a
// gatewayTransientBlip (5xx keeps its own failure class), so a terminal
// failure classifies through the no-exec-fallback DROP branch — the
// (c) outcome in the row's preferred text.
func sgap1687FailingGatewayServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"sgap1687 persistent 5xx"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSCHEDGAP1687_NoFallback_LogNamesRealOutcome — acceptance criterion 2,
// noExecFallback=true half. A gateway failure on a no-fallback spawner must
// log the real outcome (the drop: "exec fallback disabled, dropping tick")
// and must NOT log "falling back to exec" — the scheduler never execs on
// this path, and the log may not claim it does.
func TestSCHEDGAP1687_NoFallback_LogNamesRealOutcome(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const (
		projectName = "sgap1687-nofb"
		tickID      = "sgap1687-nofb-2026-09-30-00-00-01"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	buf := sgap1687CaptureLog(t)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(sgap1687FailingGatewayServer(t).URL, "***", 5*time.Second))
	spawner.SetNoExecFallback(true)
	spawner.timeout = 30 * time.Second

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — a persistent 5xx with exec fallback disabled must drop the tick")
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick — a dropped tick must not produce one")
	}
	if !strings.Contains(err.Error(), "exec fallback disabled") {
		t.Errorf("error = %q, want it to name the real outcome (exec fallback disabled)", err.Error())
	}

	logs := buf.String()
	if strings.Contains(logs, "falling back to exec") {
		t.Errorf("logs contain 'falling back to exec' — noExecFallback=true never execs; the log must name the real outcome (exec fallback disabled, dropping tick):\n%s", logs)
	}
	if !strings.Contains(logs, "exec fallback disabled, dropping tick") {
		t.Errorf("logs missing the real-outcome wording 'exec fallback disabled, dropping tick':\n%s", logs)
	}
	if !strings.Contains(logs, "SKIPPED: "+projectName+" tick="+tickID) {
		t.Errorf("logs missing 'SKIPPED: %s tick=%s' drop line:\n%s", projectName, tickID, logs)
	}
	// This shape is NOT a stall — the GAP-117 line must stay silent here, so
	// the outcome wording above is the only story the log tells.
	if strings.Contains(logs, "GATEWAY STALLED") {
		t.Errorf("logs contain 'GATEWAY STALLED' — a 5xx exhaustion is a drop, not a stalled turn:\n%s", logs)
	}
}

// TestSCHEDGAP1687_StalledWithFallbackEnabled_NeverClaimsExec — the
// fallback-ENABLED half of acceptance criterion 2, through the early-return
// branch that made the old pre-classification line wrong EVEN when exec
// fallback was on: a stalled turn (SCHED-GAP-117) returns before the exec
// block, so "falling back to exec.Command" was a false claim in both flag
// states. The log must carry only the real GATEWAY STALLED outcome.
func TestSCHEDGAP1687_StalledWithFallbackEnabled_NeverClaimsExec(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const projectName = "sgap1687-stalled-fb"
	mustCreateProjectINFRA012(t, db, projectName)

	requestSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	srv := httptest.NewServer(stallGatewayHandler(requestSeen, release))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) }) // never let srv.Close() hang
		srv.Close()
	})

	buf := sgap1687CaptureLog(t)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "***", 30*time.Second))
	spawner.SetNoExecFallback(false) // fallback ENABLED — the branch still never execs
	spawner.timeout = 30 * time.Second
	spawner.SetGatewayResponseTimeout(250 * time.Millisecond)

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, "sgap1687-stalled-fb-2026-09-30-00-00-02")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	outcome := tick.Wait()
	if outcome.Status != TickFailed {
		t.Errorf("Wait() status = %s, want %s (stalled-turn classification unchanged by the wording fix)", outcome.Status, TickFailed)
	}

	logs := buf.String()
	if strings.Contains(logs, "falling back to exec") {
		t.Errorf("logs contain 'falling back to exec' — the stalled-turn path returns before the exec block even with fallback enabled; the log must name the real outcome:\n%s", logs)
	}
	if !strings.Contains(logs, "GATEWAY STALLED: "+projectName) {
		t.Errorf("logs missing 'GATEWAY STALLED: %s' — the real outcome line:\n%s", projectName, logs)
	}
}

// TestSCHEDGAP1687_FallbackEnabledDrop_LogMaySayExec — the control arm: with
// exec fallback ENABLED the drop shape REALLY execs, so the wording must
// keep saying so. PATH is pinned to an empty dir so the exec attempt fails
// fast ("executable file not found") instead of launching a real hermes
// chat inside the test; the assertion is on the log text, which fires before
// the process start.
func TestSCHEDGAP1687_FallbackEnabledDrop_LogMaySayExec(t *testing.T) {
	gap170BootState(t)
	gap170ResetGate(t)

	db := newTestDB(t)
	const (
		projectName = "sgap1687-fb-drop"
		tickID      = "sgap1687-fb-drop-2026-09-30-00-00-03"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	// Pin PATH to an empty temp dir so exec.Command("hermes") fails at
	// LookPath inside the test process (t.Setenv also governs the child env
	// the spawn path builds from os.Environ()).
	t.Setenv("PATH", t.TempDir())

	buf := sgap1687CaptureLog(t)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(sgap1687FailingGatewayServer(t).URL, "***", 5*time.Second))
	spawner.SetNoExecFallback(false)
	spawner.timeout = 30 * time.Second

	tick, err := spawner.Spawn(PackedProject{Name: projectName, Workdir: t.TempDir()}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — the pinned PATH must make the exec attempt fail")
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick — a failed exec start must not produce one")
	}

	logs := buf.String()
	if !strings.Contains(logs, "GATEWAY FAIL: "+projectName+" tick="+tickID) {
		t.Errorf("logs missing 'GATEWAY FAIL: %s tick=%s' — the real fallback attempt must still announce itself:\n%s", projectName, tickID, logs)
	}
	if !strings.Contains(logs, "falling back to exec.Command") {
		t.Errorf("logs missing 'falling back to exec.Command' — this path DOES exec (fallback enabled), the wording must stay:\n%s", logs)
	}
}

// TestSCHEDGAP1687_ExecWordingOnlyNearExecCommand — acceptance criterion 1 as
// a static pin: exactly ONE "falling back to exec" wording line may exist in
// spawn.go (the real fallback announcement), and it must sit within 40 lines
// of an actual exec.Command call AFTER it — the classification block flows
// straight into the exec build. The old pre-classification site (~1850) sat
// ~280 lines from the call, so any regression back there breaks this guard,
// and losing the wording entirely fails the loud zero-wording arm.
func TestSCHEDGAP1687_ExecWordingOnlyNearExecCommand(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("spawn.go"))
	if err != nil {
		t.Fatalf("read spawn.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	wording := []int{}
	for i, line := range lines {
		// Comment lines may freely DISCUSS the wording (this row's own
		// annotations do); only CODE lines can emit it. The log string sits
		// inside a log.Printf call line, never on a pure comment line.
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "falling back to exec") {
			wording = append(wording, i+1) // 1-based
		}
	}
	if len(wording) != 1 {
		t.Fatalf("spawn.go carries %d 'falling back to exec' wording lines (%v), want exactly 1 — the wording may only announce the REAL exec fallback", len(wording), wording)
	}
	const window = 40
	at := wording[0] - 1 // 0-based
	for j := at + 1; j <= at+window && j < len(lines); j++ {
		if strings.Contains(lines[j], "exec.Command(") {
			return // wording is immediately followed by the real call — correct
		}
	}
	t.Errorf("spawn.go:%d says 'falling back to exec' but no exec.Command call follows within %d lines — the wording claims a mechanism this path does not run:\n	%s", wording[0], window, strings.TrimSpace(lines[at]))
}
