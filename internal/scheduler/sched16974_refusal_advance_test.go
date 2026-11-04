package scheduler

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SCHED-GAP-16974: the refusal advance shipped (9a5f5f9b, widened in 5d00e90f)
// with a classifier table test but NO spawn-level proof that a 503 refusal
// followed by a 200 actually completes the tick on hop 2. That row said so
// explicitly — "the call site is wired and reviewed, not proven end-to-end" —
// and these two tests are that proof.
//
// They assert OBSERVABLE outcomes only: the tick completes, exactly two POSTs
// are made, the SECOND carries the fallback pair, and the refusal is logged as a
// routing verdict rather than a pair rejection.

// sched16974RefusalBody is the shape the task-router uses for its own "I declined
// to route" 503s (the "no open hop" marker — one the FIRST cut of
// isRouterRefusal did not classify, found in the 20:21 tick errors).
const sched16974RefusalBody = `{"error":"no open hop for this request","chain_length":0,"ladder":[],"max_hops":3}`

// TestSCHED16974_RefusalAdvancesToNextHopAndCompletes — the acceptance test the
// row asked for: 503 refusal on hop 1, 200 on hop 2, tick completes.
func TestSCHED16974_RefusalAdvancesToNextHopAndCompletes(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap16974-refusal"
		tickID      = "gap16974-refusal-2026-11-03-22-40-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var (
		mu    sync.Mutex
		posts []string // the model field of each POST body, in order
	)
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		posts = append(posts, payload.Model)
		mu.Unlock()
		// Refuse EVERY attempt on the primary pair (the router's verdict is
		// deterministic — this is what a real unservable rating looks like), and
		// serve the fallback. Refusing only the first POST would prove nothing:
		// the bounded same-pair transient retry (SCHED-GAP-080) would "recover"
		// on attempt 2 without ever advancing the chain, which is exactly the
		// confusion this test exists to catch.
		if payload.Model == "model-one" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(sched16974RefusalBody))
			return
		}
		schedGap080CompletedResponse(w)
	}, 5*time.Second)

	tick, err := spawner.Spawn(PackedProject{
		Name:             projectName,
		Workdir:          t.TempDir(),
		Model:            "model-one",
		Provider:         "prov-one",
		FallbackModel:    "model-two",
		FallbackProvider: "prov-two",
		NoGlobalFallback: true,
	}, tickID)
	if err != nil {
		t.Fatalf("Spawn errored on a refusal the fallback could serve: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick")
	}
	if outcome := tick.Wait(); outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s — a refusal must advance to the next hop, not kill the tick",
			outcome.Status, TickCompleted)
	}

	mu.Lock()
	got := append([]string(nil), posts...)
	mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("gateway POSTs = %d (%v), want the primary refusing then the fallback serving", len(got), got)
	}
	if got[0] != "model-one" {
		t.Errorf("first POST model = %q, want model-one (the primary hop)", got[0])
	}
	if last := got[len(got)-1]; last != "model-two" {
		t.Errorf("last POST model = %q, want model-two — the chain must END on the fallback pair", last)
	}
	// Every attempt before the last one was the refused head; the advance is a
	// ONE-ATTEMPT-PER-HOP rule, so the fallback must appear exactly once.
	fallbackPosts := 0
	for _, m := range got {
		if m == "model-two" {
			fallbackPosts++
		}
	}
	if fallbackPosts != 1 {
		t.Errorf("fallback pair POSTed %d time(s) (%v), want exactly 1 (one attempt per hop)", fallbackPosts, got)
	}
	// A refusal is a deterministic verdict, so the head is tried ONCE — the
	// bounded same-pair transient retry (SCHED-GAP-080) must not spend its
	// attempts re-asking a question the router already answered.
	if len(got) != 2 || got[0] != "model-one" {
		t.Errorf("POST sequence = %v, want exactly [model-one model-two]: a refusal must not be retried on the same pair", got)
	}

	logs := buf.String()
	if !strings.Contains(logs, "GATEWAY ROUTER REFUSAL") {
		t.Errorf("logs missing the ROUTER REFUSAL line that explains the advance:\n%s", logs)
	}
	// A rating refusal is not a pair rejection: the 401/403 wording and its
	// breaker-trip sentence must not appear.
	if strings.Contains(logs, "GATEWAY FAIL") {
		t.Errorf("logs contain GATEWAY FAIL — the tick completed:\n%s", logs)
	}
	if strings.Contains(logs, "rejected (HTTP 401/403)") {
		t.Errorf("logs classify a routing refusal as a 401/403 pair rejection:\n%s", logs)
	}
}

// TestSCHED16974_OutageDoesNotAdvance — the negative half. A plain 503 carrying
// no router marker is an OUTAGE: the chain must NOT walk, because walking it
// multiplies load on a gateway that is already sick. That boundary is the whole
// reason the marker set is narrow.
func TestSCHED16974_OutageDoesNotAdvance(t *testing.T) {
	db := newTestDB(t)
	const (
		projectName = "gap16974-outage"
		tickID      = "gap16974-outage-2026-11-03-22-41-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0)

	var requests int32
	buf := schedGap080CaptureLog(t)
	spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream connect timeout"}`))
	}, 5*time.Second)

	_, err := spawner.Spawn(PackedProject{
		Name:             projectName,
		Workdir:          t.TempDir(),
		Model:            "model-one",
		Provider:         "prov-one",
		FallbackModel:    "model-two",
		FallbackProvider: "prov-two",
		NoGlobalFallback: true,
	}, tickID)
	if err == nil {
		t.Fatal("Spawn returned nil error — an upstream outage must fail the tick")
	}
	logs := buf.String()
	if strings.Contains(logs, "GATEWAY ROUTER REFUSAL") {
		t.Errorf("logs contain GATEWAY ROUTER REFUSAL for a plain outage — the marker set must stay narrow:\n%s", logs)
	}
	if got := atomic.LoadInt32(&requests); got < 1 {
		t.Errorf("gateway POSTs = %d, want at least one attempt", got)
	}
}
