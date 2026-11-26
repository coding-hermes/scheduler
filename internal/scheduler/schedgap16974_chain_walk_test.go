package scheduler

// SCHED-GAP-16974 — spawn-level fallback chain walk, both table arms.
//
// The refusal classifier (isRouterRefusal, gateway_client.go) has its own table
// test (router_refusal_internal_test.go); this file is the proof at the CALL
// SITE: the real Spawn() path against a stub gateway, not a mocked classifier.
// The chain walk in Spawn() has exactly two arms, and both are pinned here:
//
//	Arm 1 — "unservable-rating walk": HTTP 503 whose body carries the router
//	        verdict marker IS a refusal about the pair — the walk advances to
//	        the next chain entry, the tick completes on hop 2, and (because the
//	        refusal is a RATING verdict, not a dead pair) NO circuit failure is
//	        recorded. See spawn.go around the "SCHED-GAP-243" comment:
//	        "ONLY 503 + unservable-rating advances the chain" and
//	        "no breaker trip: the rating was unservable, not the pair dead".
//	Arm 2 — "no-verdict no-walk": a 503 carrying NO router marker is an
//	        OUTAGE — a transport failure, not a verdict — so the chain must NOT
//	        walk: the tick stays on the SAME pair (bounded same-pair transient
//	        retries only), fails, and the pair IS charged via record-failure
//	        ("walking the chain through an outage multiplies load on a gateway
//	        that is already sick").
//
// Both arms drive the REAL spawn path (Spawner.Spawn → sendTurn → the
// refusal/advance logic in spawn.go), with the circuit recording wired through
// the same fake-circuit capture used by the TASK-ROUTER-002 tests, so the
// "no breaker trip on a refusal" contract is observable, not inferred.

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sched16974ChainWalkRefusalBody is the real task-router "I declined to route"
// shape (failure_reason "unservable-rating") — the marker that turns a 503
// into a routing verdict.
const sched16974ChainWalkRefusalBody = `{"error": "no eligible model for this rating in the registry and no fallback lane could serve (requirements_unmet: the rating outruns the fleet)", "failure_reason": "unservable-rating", "lane_class": "primary"}`

// TestSCHED16974_ChainWalk runs BOTH table arms of the spawn-level chain walk
// against the real Spawn() path, each with its own stub gateway and its own
// circuit-capture client.
func TestSCHED16974_ChainWalk(t *testing.T) {
	t.Run("unservable-rating-walk", func(t *testing.T) {
		db := newTestDB(t)
		const (
			projectName = "gap16974-walk"
			tickID      = "gap16974-walk-2026-11-26-12-00-00"
		)
		mustCreateProjectINFRA012(t, db, projectName)
		insertRunningTick(t, db, tickID, projectName, 0)

		var (
			mu    sync.Mutex
			pairs []string // "model@provider" of each POST, in order
		)
		buf := schedGap080CaptureLog(t)
		spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Model    string `json:"model"`
				Provider string `json:"provider"`
			}
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			pairs = append(pairs, payload.Model+"@"+payload.Provider)
			mu.Unlock()
			// The refusal is deterministic: EVERY attempt on the primary pair
			// gets the same verdict, so a same-pair transient retry could never
			// "recover" it (and SCHED-GAP-16974 stopped wasting attempts that
			// way anyway). Hop 2 serves.
			if payload.Model == "model-one" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(sched16974ChainWalkRefusalBody))
				return
			}
			schedGap080CompletedResponse(w)
		}, 5*time.Second)
		// Wire the circuit recording so the "no breaker trip" contract is
		// observable: a refusal is a RATING verdict, so record-failure must
		// never fire for the refused head.
		capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
		spawner.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))

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
			t.Errorf("Wait() status = %s, want %s — the refusal must walk to hop 2 and complete",
				outcome.Status, TickCompleted)
		}

		// Exactly two dispatches, in chain order: hop 0 (the refused head),
		// then hop 1 (the serving pair). No same-pair retry, no extra hops.
		mu.Lock()
		got := append([]string(nil), pairs...)
		mu.Unlock()
		if len(got) != 2 {
			t.Fatalf("dispatch count = %d (%v), want exactly 2 (refused head + serving hop)", len(got), got)
		}
		if got[0] != "model-one@prov-one" {
			t.Errorf("first dispatch = %q, want model-one@prov-one (hop 0)", got[0])
		}
		if got[1] != "model-two@prov-two" {
			t.Errorf("second dispatch = %q, want model-two@prov-two (hop 1 — chain order)", got[1])
		}

		// The ROW records both hops with the tick's hop list indexes, and the
		// completed attempt names the SECOND pair — the pair that actually ran.
		attempts := readTickHopAttempts(t, db, tickID)
		if len(attempts) != 2 {
			t.Fatalf("gateway_attempts rows = %d, want 2", len(attempts))
		}
		if attempts[0].Hop != 0 || attempts[0].Model != "model-one" || attempts[0].Provider != "prov-one" {
			t.Errorf("attempts[0] = %+v, want hop 0 model-one/prov-one (the refused head)", attempts[0])
		}
		if attempts[0].Reason != hopReasonGateway {
			t.Errorf("attempts[0].reason = %q, want %q (a refusal is an answered gateway error, not a stall)",
				attempts[0].Reason, hopReasonGateway)
		}
		if attempts[1].Hop != 1 || attempts[1].Model != "model-two" || attempts[1].Provider != "prov-two" {
			t.Errorf("attempts[1] = %+v, want hop 1 model-two/prov-two (the pair that ran)", attempts[1])
		}
		if attempts[1].Reason != hopReasonCompleted {
			t.Errorf("attempts[1].reason = %q, want %q", attempts[1].Reason, hopReasonCompleted)
		}

		// NO circuit failure for a router refusal: the breaker must not charge
		// a pair for the rating layer's verdict. The only circuit call on this
		// path is record-success for the pair that actually ran.
		calls := readCircuitCalls(t, capture)
		if len(calls) != 1 {
			t.Fatalf("circuit invocations = %v, want exactly 1 (record-success for hop 2)", calls)
		}
		if calls[0] != "record-success prov-two model-two" {
			t.Errorf("circuit call = %q, want %q", calls[0], "record-success prov-two model-two")
		}
		if strings.Contains(buf.String(), "GATEWAY FAIL") {
			t.Errorf("logs contain GATEWAY FAIL — the tick completed on hop 2:\n%s", buf.String())
		}
	})

	t.Run("no-verdict-no-walk", func(t *testing.T) {
		db := newTestDB(t)
		const (
			projectName = "gap16974-nowalk"
			tickID      = "gap16974-nowalk-2026-11-26-12-01-00"
		)
		mustCreateProjectINFRA012(t, db, projectName)
		insertRunningTick(t, db, tickID, projectName, 0)

		var (
			mu    sync.Mutex
			pairs []string
		)
		buf := schedGap080CaptureLog(t)
		spawner := schedGap080Spawner(t, db, buf, func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Model    string `json:"model"`
				Provider string `json:"provider"`
			}
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			pairs = append(pairs, payload.Model+"@"+payload.Provider)
			mu.Unlock()
			// A 503 with NO router marker: an outage, not a verdict. The chain
			// must never walk through it.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"upstream connect timeout"}`))
		}, 5*time.Second)
		capture := filepath.Join(t.TempDir(), "circuit_calls.txt")
		spawner.SetCircuitClient(NewCircuitClient([]string{writeFakeCircuit(t, capture)}, 0))

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
			t.Fatal("Spawn returned nil error — an outage must fail the tick")
		}
		if !strings.Contains(err.Error(), "HTTP 503") {
			t.Errorf("error = %q, want it to carry the real HTTP 503", err.Error())
		}
		if got := tickStatusOf(t, db, tickID); got == "completed" {
			t.Errorf("tick row status = completed — a no-verdict outage must never complete")
		}

		// The tick STAYS PUT on the same pair: only the bounded same-pair
		// transient retry (SCHED-GAP-080) re-asks, hop 1 of the chain is never
		// dispatched.
		mu.Lock()
		got := append([]string(nil), pairs...)
		mu.Unlock()
		want := 1 + gatewayRetryDefaultMax
		if len(got) != want {
			t.Fatalf("dispatch count = %d (%v), want %d (1 initial + %d bounded same-pair retries)", len(got), got, want, gatewayRetryDefaultMax)
		}
		for i, p := range got {
			if p != "model-one@prov-one" {
				t.Errorf("dispatch[%d] = %q, want model-one@prov-one — the chain must NOT walk on a no-verdict outage", i, p)
			}
		}
		// The row agrees: every attempt is hop 0, the refused-by-outage pair.
		attempts := readTickHopAttempts(t, db, tickID)
		if len(attempts) != want {
			t.Fatalf("gateway_attempts rows = %d, want %d", len(attempts), want)
		}
		for i, a := range attempts {
			if a.Hop != 0 || a.Model != "model-one" || a.Provider != "prov-one" {
				t.Errorf("attempts[%d] = %+v, want hop 0 model-one/prov-one (never advanced)", i, a)
			}
			if a.Reason != hopReasonTransport {
				t.Errorf("attempts[%d].reason = %q, want %q (a plain 5xx is a transport failure)", i, a.Reason, hopReasonTransport)
			}
		}
		// Contrast with arm 1: an outage IS charged — the pair that died gets
		// its record-failure so the breaker can cool it across ticks.
		calls := readCircuitCalls(t, capture)
		if len(calls) == 0 {
			t.Fatal("circuit invocations = 0, want at least one record-failure for the outage pair")
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "record-success") {
				t.Errorf("circuit call %q — nothing succeeded on this tick, record-success must never fire", c)
			}
		}
		found := false
		for _, c := range calls {
			if strings.HasPrefix(c, "record-failure prov-one model-one ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("circuit calls %v missing record-failure for the outage pair (prov-one, model-one)", calls)
		}
		if strings.Contains(buf.String(), "GATEWAY ROUTER REFUSAL") {
			t.Errorf("logs contain GATEWAY ROUTER REFUSAL for a plain outage — the marker set must stay narrow:\n%s", buf.String())
		}
	})
}

// readTickHopAttempts reads the persisted ticks.gateway_attempts JSON array
// (SCHED-GAP-16939, migration v79) for the given tick.
func readTickHopAttempts(t *testing.T, db *sql.DB, tickID string) []hopAttempt {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(gateway_attempts,'') FROM ticks WHERE id = ?`, tickID).Scan(&raw); err != nil {
		t.Fatalf("read gateway_attempts: %v", err)
	}
	if raw == "" {
		t.Fatal("ticks.gateway_attempts is empty — the spawn path never persisted its hop history")
	}
	var rows []hopAttempt
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("gateway_attempts is not a JSON array: %v (%s)", err, raw)
	}
	return rows
}
