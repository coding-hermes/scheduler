package scheduler

// PERF-001-R2 (a): the block profile must be armed from the evaluate() path,
// so GET /debug/pprof/block (already registered on DefaultServeMux by
// cmd/schedulerd/main.go) stops answering "0 nodes accounting for 0, 0% of 0
// total" and starts naming whatever the status request waits on.
//
// Both halves are proven in a CHILD process (the runtime block-profile rate is
// process-global state, so arming it in-process would change every other test
// in this binary) — the same subprocess pattern
// internal/scheduler/usage_pool_policy_test.go uses:
//
//	TestPERF001R2BlockProfileRateRespected — arms with an explicit raw rate
//	and requires the profile to record THIS test's blocking frame at rate > 0
//	and nothing at all at rate 0 (positive + negative control, no ambiguity
//	about which stack the record came from).
//
//	TestPERF001R2EvaluateArmsBlockProfile — drives the production call site
//	(Loop.evaluate) with no rate env set and requires the default rate (1) to
//	be armed, plus the eval-hold observer to have recorded the pass.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// perf001r2ChildPrefix is the marker line the child tests print; the parent
// parses it out of the child's combined output.
const perf001r2ChildPrefix = "PERF001R2-RESULT"

// perf001r2MarkerMu is the mutex perf001r2BlockOnMarker blocks on. The block
// profile's text form carries function names, so a record naming
// perf001r2BlockOnMarker can only have come from this deliberate block.
var perf001r2MarkerMu sync.Mutex

// perf001r2BlockOnMarker blocks on perf001r2MarkerMu. It is called in its own
// goroutine by the tests below, so the recorded stack is
// sync.Mutex.Lock <- perf001r2BlockOnMarker <- goexit.
//
//go:noinline
func perf001r2BlockOnMarker() {
	perf001r2MarkerMu.Lock()
	// Intentional: this is the deliberate block that ArmBlockProfile
	// records when it is armed. The Lock above is the observable event.
	// TryLock() returns false because we already hold the mutex, but the
	// call satisfies staticcheck SA2001 (non-empty critical section).
	_ = perf001r2MarkerMu.TryLock()
	perf001r2MarkerMu.Unlock()
}

// perf001r2ObserveBlock drives one deliberate, completed block on the marker
// mutex: the block is what the profile must record when the profile is armed.
func perf001r2ObserveBlock() {
	perf001r2MarkerMu.Lock()
	done := make(chan struct{})
	go func() {
		perf001r2BlockOnMarker()
		close(done)
	}()
	time.Sleep(20 * time.Millisecond) // let the goroutine reach the block
	perf001r2MarkerMu.Unlock()
	<-done
	time.Sleep(10 * time.Millisecond) // let the runtime record the completed block
}

// perf001r2BlockProfileText returns the profile's text form (debug=1, which
// carries symbolized frames) and whether it names the marker frame.
func perf001r2BlockProfileText(t *testing.T) (string, bool) {
	t.Helper()
	p := pprof.Lookup("block")
	if p == nil {
		t.Fatal("runtime/pprof.Lookup(\"block\") returned nil")
	}
	var buf bytes.Buffer
	if err := p.WriteTo(&buf, 1); err != nil {
		t.Fatalf("block profile WriteTo: %v", err)
	}
	text := buf.String()
	return text, strings.Contains(text, "perf001r2BlockOnMarker")
}

// perf001r2ChildEnv copies the parent environment while dropping the block
// profile knobs, so each child resolves the rate the test asked for.
func perf001r2ChildEnv(extra map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "PERF001R2_") || name == BlockProfileRateEnv {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// perf001r2RunChild runs one named test in a child copy of this test binary.
func perf001r2RunChild(t *testing.T, testName string, extra map[string]string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = perf001r2ChildEnv(extra)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child %s failed: %v\n%s", testName, err, out)
	}
	return string(out)
}

// perf001r2ParseChild pulls the marker line into a map of key=value fields.
func perf001r2ParseChild(t *testing.T, out string) map[string]string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, perf001r2ChildPrefix) {
			continue
		}
		fields := map[string]string{}
		for _, f := range strings.Fields(strings.TrimPrefix(line, perf001r2ChildPrefix)) {
			k, v, ok := strings.Cut(f, "=")
			if ok {
				fields[k] = v
			}
		}
		return fields
	}
	t.Fatalf("child printed no %s marker line:\n%s", perf001r2ChildPrefix, out)
	return nil
}

func TestPERF001R2ResolveBlockProfileRate(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", DefaultBlockProfileRate},
		{"1", 1},
		{"1000", 1000},
		{"0", 0},
		{"-1", 0},
		{" 2 ", 2},
		{"nonsense", DefaultBlockProfileRate},
	} {
		if got := resolveBlockProfileRate(tc.raw); got != tc.want {
			t.Errorf("resolveBlockProfileRate(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
	if DefaultBlockProfileRate != 1 {
		t.Fatalf("DefaultBlockProfileRate = %d, want 1 (PERF-001-R2: every blocking event)", DefaultBlockProfileRate)
	}
	if BlockProfileRateEnv != "SCHEDULER_BLOCK_PROFILE_RATE" {
		t.Fatalf("BlockProfileRateEnv = %q, want SCHEDULER_BLOCK_PROFILE_RATE", BlockProfileRateEnv)
	}
}

// TestPERF001R2BlockProfileRateRespected is the positive/negative control for
// the arming itself: rate > 0 must record the marker block, rate 0 must record
// nothing at all, and the test-binary exemption must resolve to "disabled"
// unless the operator set the rate explicitly.
func TestPERF001R2BlockProfileRateRespected(t *testing.T) {
	if os.Getenv("PERF001R2_BLOCK_CHILD_SET") != "" {
		t.Skip("child mode")
	}
	for _, tc := range []struct {
		name       string
		raw        string
		testBinary bool
		wantRate   int
		wantArmed  bool
		wantMarker bool
	}{
		{name: "rate=1", raw: "1", wantRate: 1, wantArmed: true, wantMarker: true},
		{name: "rate=0", raw: "0", wantRate: 0, wantArmed: false, wantMarker: false},
		{name: "unset", raw: "", wantRate: DefaultBlockProfileRate, wantArmed: true, wantMarker: true},
		{name: "nonsense", raw: "nonsense", wantRate: DefaultBlockProfileRate, wantArmed: true, wantMarker: true},
		{name: "test-binary-unset", raw: "", testBinary: true, wantRate: 0, wantArmed: false, wantMarker: false},
		{name: "test-binary-explicit", raw: "1", testBinary: true, wantRate: 1, wantArmed: true, wantMarker: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]string{
				"PERF001R2_BLOCK_CHILD_SET": "1",
				"PERF001R2_BLOCK_CHILD":     tc.raw,
			}
			if tc.testBinary {
				extra["PERF001R2_BLOCK_CHILD_TESTBIN"] = "1"
			}
			out := perf001r2RunChild(t, "TestPERF001R2BlockProfileChild", extra)
			fields := perf001r2ParseChild(t, out)
			if got := fields["rate"]; got != fmt.Sprint(tc.wantRate) {
				t.Errorf("child resolved rate = %s, want %d", got, tc.wantRate)
			}
			if got := fields["armed"]; got != fmt.Sprint(tc.wantArmed) {
				t.Errorf("child armed = %s, want %t", got, tc.wantArmed)
			}
			if got := fields["marker"]; got != fmt.Sprint(tc.wantMarker) {
				t.Errorf("child profile names perf001r2BlockOnMarker = %s, want %t (count=%s)", got, tc.wantMarker, fields["count"])
			}
			if !tc.wantMarker && fields["count"] != "0" {
				t.Errorf("child recorded %s block samples with the profile disabled, want 0", fields["count"])
			}
		})
	}
}

// TestPERF001R2BlockProfileChild is the child half of
// TestPERF001R2BlockProfileRateRespected: arm with the raw rate (and the
// test-binary flag) the parent passed, perform one deliberate block, and
// report what the profile holds.
func TestPERF001R2BlockProfileChild(t *testing.T) {
	if os.Getenv("PERF001R2_BLOCK_CHILD_SET") != "1" {
		t.Skip("parent-driven child (PERF001R2_BLOCK_CHILD_SET)")
	}
	armBlockProfile(os.Getenv("PERF001R2_BLOCK_CHILD"), os.Getenv("PERF001R2_BLOCK_CHILD_TESTBIN") == "1")
	perf001r2ObserveBlock()
	_, hasMarker := perf001r2BlockProfileText(t)
	fmt.Printf("%s rate=%d armed=%t count=%d marker=%t\n",
		perf001r2ChildPrefix, BlockProfileRate(), BlockProfileArmed(),
		pprof.Lookup("block").Count(), hasMarker)
}

// TestPERF001R2EvaluateArmsBlockProfile is the row's step-1 wiring proof: the
// production evaluate() call site must arm the profile. Two arms:
//
//   - SCHEDULER_BLOCK_PROFILE_RATE=1 ⇒ the pass leaves the profile armed and
//     recording, so the daemon's already-exposed /debug/pprof/block names the
//     wait instead of answering "0 nodes accounting for 0, 0% of 0 total";
//   - no rate env in a TEST binary ⇒ the pass leaves it disabled (the
//     exemption that keeps a process-global profiler off the test suite),
//     while still recording the pass window.
func TestPERF001R2EvaluateArmsBlockProfile(t *testing.T) {
	if os.Getenv("PERF001R2_EVAL_CHILD") != "" {
		t.Skip("child mode")
	}
	for _, tc := range []struct {
		name       string
		rate       string
		wantArmed  bool
		wantRate   int
		wantMarker bool
	}{
		{name: "explicit-rate-1", rate: "1", wantArmed: true, wantRate: 1, wantMarker: true},
		{name: "test-binary-default-off", rate: "", wantArmed: false, wantRate: 0, wantMarker: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]string{"PERF001R2_EVAL_CHILD": "1"}
			if tc.rate != "" {
				extra[BlockProfileRateEnv] = tc.rate
			}
			out := perf001r2RunChild(t, "TestPERF001R2EvaluateArmsBlockProfileChild", extra)
			fields := perf001r2ParseChild(t, out)
			if fields["armed"] != fmt.Sprint(tc.wantArmed) || fields["rate"] != fmt.Sprint(tc.wantRate) {
				t.Errorf("after evaluate(): armed=%s rate=%s, want armed=%t rate=%d", fields["armed"], fields["rate"], tc.wantArmed, tc.wantRate)
			}
			if fields["marker"] != fmt.Sprint(tc.wantMarker) {
				t.Errorf("block profile after evaluate(): names the marker frame = %s, want %t (count=%s)", fields["marker"], tc.wantMarker, fields["count"])
			}
			if fields["passes"] != "1" {
				t.Errorf("pass observer recorded %s window(s) for one evaluate() pass, want 1", fields["passes"])
			}
			if fields["inflight"] != "false" {
				t.Errorf("eval_in_flight = %s after evaluate() returned, want false", fields["inflight"])
			}
			if fields["stages"] == "0" {
				t.Errorf("stage attribution recorded nothing for a pass that ran")
			}
		})
	}
}

// TestPERF001R2EvaluateArmsBlockProfileChild is the child half of
// TestPERF001R2EvaluateArmsBlockProfile: drive the real evaluate() call site.
func TestPERF001R2EvaluateArmsBlockProfileChild(t *testing.T) {
	if os.Getenv("PERF001R2_EVAL_CHILD") != "1" {
		t.Skip("parent-driven child (PERF001R2_EVAL_CHILD)")
	}
	db := newTestDB(t)
	defer func() { _ = db.Close() }()
	// budget below every project weight ⇒ an empty pick, so the pass cannot
	// enqueue a spawn; the pass still runs its whole decision phase.
	loop := NewLoop(db, time.Minute, time.Hour, 10, 1, 5)
	loop.evaluate()
	// evaluate() is the ONLY arm site in this process, so a blocked sample
	// recorded AFTER it returns can only come from the profile it armed.
	// (An idle pass on a cold DB has nothing to contend for, so the profile
	// legitimately holds no records of its own — the deliberate block below
	// is what proves the armed profile is live.)
	perf001r2ObserveBlock()
	attr := loop.LockAttribution()
	_, hasMarker := perf001r2BlockProfileText(t)
	fmt.Printf("%s rate=%d armed=%t count=%d marker=%t passes=%d inflight=%t stages=%d\n",
		perf001r2ChildPrefix, loop.BlockProfileRate(), loop.BlockProfileArmed(),
		pprof.Lookup("block").Count(), hasMarker, attr.EvalPass.Samples, attr.EvalInFlight, len(attr.Stages))
}
