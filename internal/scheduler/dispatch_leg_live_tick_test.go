package scheduler

// SCHED-GAP-1710 — LIVE, TICK-ORIGINATED proof of the dispatch leg.
//
// WHY THIS FILE EXISTS, SEPARATE FROM dispatch_leg_live_test.go. That file
// proves the WIRE: it calls bus.Client.Dispatch directly — which is also what
// the standalone cmd/scheduler-dispatch CLI does — so on its own it cannot
// answer the question SCHED-GAP-1710 was filed against: "does a TICK hand work
// to a named agent?" A caller that reaches the verb without going through the
// tick funnel proves the verb, not the wiring.
//
// This file proves the WIRING, live: it drives the SHIPPED tick funnel —
// SlotPool.Spawn, the same entry the packer, the API spawn endpoint, orphan
// resume and wave resume all use — against a RUNNING relay, with a real NAMED
// AGENT on the other end that receives the hand-out out of its OWN inbox, ACTS
// (writes a real artifact), and answers into the scheduler's inbox. The shipped
// receive leg must correlate that answer back to the ORIGINATING TICK's
// correlation id and complete the tick.
//
// It is skipped unless a live relay is named, so `go test ./...` stays
// hermetic:
//
//	CRIER_LIVE_URL=http://127.0.0.1:8767 CRIER_LIVE_TOKEN=<relay bearer> \
//	  go test ./internal/scheduler/ -run TestLiveTickDispatchLeg -v -count=1
//
// Both throwaway agents are unregistered in cleanup.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// liveAgentGet performs one signed, agent-scoped GET. The signature covers the
// path WITHOUT the query string (the relay verifies the path it routes on).
// It returns errors instead of calling t.Fatalf so it is safe off the test
// goroutine.
func liveAgentGet(baseURL, token, agentID, path string, priv ed25519.PrivateKey) (int, []byte, error) {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	signedPath := path
	if i := strings.IndexByte(signedPath, '?'); i >= 0 {
		signedPath = signedPath[:i]
	}
	sig := ed25519.Sign(priv, []byte("GET\n"+signedPath+"\n"+ts))
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Agent-Ts", ts)
	req.Header.Set("X-Agent-Sig", hex.EncodeToString(sig))
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// liveAgentPost performs one bearer-authenticated POST (the relay's inbox
// deliver route). Off-goroutine safe: errors, never t.Fatalf.
func liveAgentPost(baseURL, token, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// runLiveTickAgent is the NAMED AGENT on the far side of the hand-out. It
// polls its OWN inbox, RECEIVES the work, ACTS on it (a real artifact whose
// content pins the correlation id and a hash of the delivered bytes), and
// ANSWERS into the scheduler's inbox in the shape the fleet's proven
// dispatcher uses. Its outcome is reported on the channel, never by t.Fatalf.
func runLiveTickAgent(result chan<- error, baseURL, token, agentID string, priv ed25519.PrivateKey, replyTo, actPath string) {
	defer func() {
		if r := recover(); r != nil {
			result <- fmt.Errorf("live agent panicked: %v", r)
		}
	}()

	var msgID, payloadB64 string
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		st, raw, err := liveAgentGet(baseURL, token, agentID,
			"/agents/"+agentID+"/inbox?limit=10&lease_seconds=120", priv)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if st != http.StatusOK {
			result <- fmt.Errorf("agent inbox retrieve: status %d body %s", st, raw)
			return
		}
		var inbox struct {
			Messages []struct {
				ID      string `json:"id"`
				Payload string `json:"payload"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &inbox); err != nil {
			result <- fmt.Errorf("agent inbox decode: %v (%s)", err, raw)
			return
		}
		if len(inbox.Messages) > 0 {
			msgID = inbox.Messages[0].ID
			payloadB64 = inbox.Messages[0].Payload
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if msgID == "" {
		result <- errors.New("the agent never received a hand-out in its own inbox")
		return
	}

	decoded, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		result <- fmt.Errorf("hand-out payload not base64: %v", err)
		return
	}
	var item map[string]any
	if err := json.Unmarshal(decoded, &item); err != nil {
		result <- fmt.Errorf("hand-out payload not JSON: %v (%s)", err, decoded)
		return
	}

	// The payload law: lane + reference + correlation id, and NEVER a task.
	if item["kind"] != bus.KindWorkDispatch {
		result <- fmt.Errorf("hand-out kind = %v, want %s", item["kind"], bus.KindWorkDispatch)
		return
	}
	if _, hasTask := item["task"]; hasTask {
		result <- errors.New("hand-out carried a task id — the payload law forbids it")
		return
	}
	lane, _ := item["lane"].(string)
	corr, _ := item["corr_id"].(string)
	if lane == "" || corr == "" {
		result <- fmt.Errorf("hand-out missing lane/corr_id: %v", item)
		return
	}

	// ACT: a real artifact, content-pinned to this hand-out.
	sum := sha256.Sum256(decoded)
	artifact := fmt.Sprintf("agent=%s\nlane=%s\ncorr_id=%s\nmessage_id=%s\npayload_sha256=%x\n",
		agentID, lane, corr, msgID, sum)
	if err := os.WriteFile(actPath, []byte(artifact), 0o644); err != nil {
		result <- fmt.Errorf("agent act (write artifact): %v", err)
		return
	}

	// ANSWER: in_reply_to names the relay message id; corr_id and the echoed
	// hand-out also ride along, so either correlation identity resolves.
	answer := map[string]any{
		"ok":          true,
		"agent":       agentID,
		"in_reply_to": msgID,
		"corr_id":     corr,
		"task":        item,
		"reply": "LIVE TICK AGENT ANSWER: lane " + lane + " handled for corr " + corr +
			"; artifact " + filepath.Base(actPath) + " written",
	}
	body, _ := json.Marshal(map[string]any{
		"payload":         answer,
		"sender":          agentID,
		"request_id":      "reply-" + corr,
		"idempotency_key": "reply-" + corr,
	})
	st, raw, err := liveAgentPost(baseURL, token, "/agents/"+replyTo+"/inbox", body)
	if err != nil {
		result <- fmt.Errorf("agent answer delivery: %v", err)
		return
	}
	if st != http.StatusCreated && st != http.StatusOK && st != http.StatusAccepted {
		result <- fmt.Errorf("agent answer delivery: status %d body %s", st, raw)
		return
	}
	result <- nil
}

// TestLiveTickDispatchLeg_ATickHandsWorkToNamedAgentAndCorrelatesTheAnswer is
// SCHED-GAP-1710's acceptance, observed from a TICK against the RUNNING relay:
//
//	the tick funnel hands ONE unit of work to ONE named agent (lane + board/
//	workdir reference + correlation id, never a task id); the agent receives it
//	out of its own inbox, ACTS, and answers; the shipped receive leg ties the
//	answer back to the ORIGINATING TICK's correlation id and completes the tick.
func TestLiveTickDispatchLeg_ATickHandsWorkToNamedAgentAndCorrelatesTheAnswer(t *testing.T) {
	relay := liveDispatchRelayFromEnv(t)

	// Two throwaway identities: the NAMED AGENT that receives the work, and
	// the scheduler's own inbox the answer comes back to.
	targetPub, targetPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen target: %v", err)
	}
	inboxPub, inboxPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen inbox: %v", err)
	}
	stamp := time.Now().UnixNano()
	targetID := fmt.Sprintf("sched-1710-livetick-agent-%d", stamp)
	inboxID := fmt.Sprintf("sched-1710-livetick-inbox-%d", stamp)
	registerLiveAgent(t, relay, targetID, targetPub, []string{"sched.dispatch.1710"})
	registerLiveAgent(t, relay, inboxID, inboxPub, []string{"sched.dispatch.1710"})
	t.Cleanup(func() {
		if st, raw := relay.signed(t, http.MethodDelete, "/agents/"+targetID, targetID, targetPriv); st != http.StatusNoContent {
			t.Logf("CLEANUP WARNING: target agent %s not unregistered (%d %s)", targetID, st, raw)
		}
		if st, raw := relay.signed(t, http.MethodDelete, "/agents/"+inboxID, inboxID, inboxPriv); st != http.StatusNoContent {
			t.Logf("CLEANUP WARNING: inbox agent %s not unregistered (%d %s)", inboxID, st, raw)
		}
	})

	// The NAMED AGENT: receives, acts, answers. Started FIRST so it is already
	// polling its inbox when the tick hands the work out.
	actPath := filepath.Join(t.TempDir(), "tick-agent-acted.txt")
	agentResult := make(chan error, 1)
	go runLiveTickAgent(agentResult, relay.url, relay.token, targetID, targetPriv, inboxID, actPath)

	// The TICK RIG — the shipped funnel, the REAL relay, and a recording
	// gateway that must see ZERO requests (no fallback).
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "helix")
	targetFile := writeDispatchTargets(t, `{"lane":"helix","agent":"`+targetID+`","workdir":"/agent/helix"}`+"\n")
	t.Setenv(EnvDispatchTargets, targetFile)

	var gatewayHits int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&gatewayHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gw.Close)

	loop := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	client := bus.NewClient(true, relay.url, relay.token, "live-tick-1710")
	client.SetAgentIdentity(inboxID, inboxPriv)
	loop.SetSchedulerBus(NewSchedulerBus(client))
	pool := loop.slotPool
	pool.spawner.SetGatewayClient(NewGatewayClient(gw.URL, "k", 5*time.Second))
	pool.spawner.SetNoExecFallback(true)

	// ★ THE TICK. SlotPool.Spawn is the single funnel every entry point uses.
	tickID := pool.Spawn(PackedProject{Name: "helix", Workdir: "/agent/helix"}, time.Now(), true, db)
	t.Logf("LIVE TICK %s handed out on lane helix → agent %s", tickID, targetID)

	row := waitForTerminalTick(t, db, "helix", 3*time.Minute)
	if row.Status != "completed" {
		t.Fatalf("tick %s status = %q (error %q), want completed via the agent's answer", row.ID, row.Status, row.Err)
	}
	if row.Err != "" {
		t.Errorf("completed tick %s carries error %q, want empty", row.ID, row.Err)
	}

	// 1 · The receipt: recorded against THIS tick, naming the agent USED and
	//     the correlation id the reply must resolve to.
	rec, okRec, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !okRec {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, okRec, err)
	}
	if rec.Agent != targetID {
		t.Errorf("receipt agent = %q, want the named agent %q", rec.Agent, targetID)
	}
	if rec.State != database.DispatchStateReplied {
		t.Errorf("receipt state = %q, want replied", rec.State)
	}
	if rec.CorrID == "" || rec.MessageID == "" || rec.Transport != "inbox" {
		t.Errorf("receipt = %+v, want a corr id + relay message id + inbox transport", rec)
	}
	if rec.Lane != "helix" {
		t.Errorf("receipt lane = %q, want helix", rec.Lane)
	}
	if !strings.Contains(rec.Reply, "LIVE TICK AGENT ANSWER") {
		t.Errorf("receipt reply = %q, want the agent's answer", rec.Reply)
	}

	// 2 · The correlation resolves to the ORIGINATING TICK.
	byCorr, found, err := database.TickDispatchByCorrID(context.Background(), db, rec.CorrID)
	if err != nil || !found {
		t.Fatalf("corr %s did not resolve: found=%v err=%v", rec.CorrID, found, err)
	}
	if byCorr.TickID != row.ID {
		t.Errorf("corr %s resolved to tick %s, want the originating tick %s", rec.CorrID, byCorr.TickID, row.ID)
	}

	// 3 · The agent really ACTED, and its artifact is pinned to this hand-out.
	actRaw, err := os.ReadFile(actPath)
	if err != nil {
		t.Fatalf("the agent's artifact is missing — it never acted: %v", err)
	}
	if !strings.Contains(string(actRaw), rec.CorrID) {
		t.Errorf("agent artifact does not carry corr %s: %s", rec.CorrID, actRaw)
	}

	// 4 · NO FALLBACK: not one request reached the shared gateway.
	if n := atomic.LoadInt64(&gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests — a remote lane must never fall back", n)
	}

	if err := <-agentResult; err != nil {
		t.Fatalf("named agent: %v", err)
	}

	t.Logf("LIVE PROOF — TICK_ID=%s CORR_ID=%s MESSAGE_ID=%s AGENT=%s REPLY=%q",
		row.ID, rec.CorrID, rec.MessageID, rec.Agent, rec.Reply)
}

// TestDispatchDefault_NoTargetFileTakesTheLocalPath pins the ADDITIVE
// requirement of SCHED-GAP-1710 hermetically: a fleet that declares no
// execution target must behave exactly as before — the tick goes down the
// ordinary local spawn path and NO dispatch receipt is ever written. A missing
// file (or a lane absent from it) is "no remote lanes", not an error and not a
// silent hand-out.
func TestDispatchDefault_NoTargetFileTakesTheLocalPath(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "helix")

	// The target file does not exist: exactly the state of a fleet that
	// dispatches nothing.
	t.Setenv(EnvDispatchTargets, filepath.Join(t.TempDir(), "absent.jsonl"))
	if _, ok, err := LookupDispatchTarget(DefaultDispatchTargetsPath(), "helix"); err != nil || ok {
		t.Fatalf("lane helix resolved remote=%v err=%v with no target file, want the additive local default", ok, err)
	}

	loop := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	pool := loop.slotPool
	// No gateway client + no exec fallback: the LOCAL path drops the tick
	// immediately, so nothing in this test touches the network. If the tick
	// had taken the DISPATCH path it would instead have refused for a missing
	// bus/inbox identity — and, more to the point, it would have written a
	// tick_dispatch receipt.
	pool.spawner.SetNoExecFallback(true)

	tickID := pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)
	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.ID != tickID {
		t.Errorf("terminal tick %s != spawned tick %s", row.ID, tickID)
	}

	// THE ADDITIVE PROPERTY: the tick ran the local path, so no dispatch
	// receipt exists for it.
	if _, ok, err := database.TickDispatchForTick(context.Background(), db, tickID); err != nil {
		t.Fatalf("read tick_dispatch for %s: %v", tickID, err)
	} else if ok {
		t.Errorf("tick %s (status %q) wrote a dispatch receipt — a lane with no target must never dispatch", tickID, row.Status)
	}
}
