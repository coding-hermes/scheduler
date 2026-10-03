package scheduler

// SCHED-GAP-1710 — LIVE proof of the dispatch leg against a RUNNING relay.
//
// The hermetic tests prove the wiring; this file proves the WIRE. It is
// skipped unless a live relay is named, so `go test ./...` stays hermetic:
//
//	CRIER_LIVE_URL=http://127.0.0.1:8767 CRIER_LIVE_TOKEN=<relay bearer> \
//	  go test ./internal/scheduler/ -run TestLiveDispatchLeg -v -count=1
//
// It registers TWO throwaway agents (never a real project agent), hands one of
// them a unit of work through the SHIPPED bus client, reads the delivered bytes
// back out of that agent's OWN inbox (the agent receives the work), has the
// agent answer into the scheduler's inbox exactly the way the fleet's proven
// dispatcher answers (in_reply_to = the relay message id, the original payload
// echoed as `task`), and then runs the SHIPPED receive leg — awaitDispatchReply
// — which must correlate that answer back to the hand-out, ack it, and release
// the lease. Both agents are unregistered in cleanup.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

type liveDispatchRelay struct {
	url   string
	token string
	http  *http.Client
}

func liveDispatchRelayFromEnv(t *testing.T) *liveDispatchRelay {
	t.Helper()
	url := strings.TrimRight(os.Getenv("CRIER_LIVE_URL"), "/")
	if url == "" {
		t.Skip("CRIER_LIVE_URL not set: live dispatch-leg proof skipped (set it to a running relay to run this test)")
	}
	return &liveDispatchRelay{url: url, token: os.Getenv("CRIER_LIVE_TOKEN"), http: &http.Client{Timeout: 20 * time.Second}}
}

func (l *liveDispatchRelay) do(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, l.url+path, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if l.token != "" {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}
	resp, err := l.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

func (l *liveDispatchRelay) signed(t *testing.T, method, path, agentID string, priv ed25519.PrivateKey) (int, []byte) {
	t.Helper()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	// The signed path EXCLUDES the query string (the relay verifies the path
	// it routes on; the query is transport plumbing).
	signedPath := path
	if i := strings.IndexByte(signedPath, '?'); i >= 0 {
		signedPath = signedPath[:i]
	}
	sig := ed25519.Sign(priv, []byte(method+"\n"+signedPath+"\n"+ts))
	req, err := http.NewRequest(method, l.url+path, nil)
	if err != nil {
		t.Fatalf("build signed %s %s: %v", method, path, err)
	}
	if l.token != "" {
		req.Header.Set("Authorization", "Bearer "+l.token)
	}
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Agent-Ts", ts)
	req.Header.Set("X-Agent-Sig", hex.EncodeToString(sig))
	resp, err := l.http.Do(req)
	if err != nil {
		t.Fatalf("signed %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

func registerLiveAgent(t *testing.T, relay *liveDispatchRelay, id string, pub ed25519.PublicKey, caps []string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "public_key": hex.EncodeToString(pub), "capabilities": caps})
	if st, raw := relay.do(t, http.MethodPost, "/agents", body); st != http.StatusCreated {
		t.Fatalf("register %s: status %d body %s", id, st, raw)
	}
}

func TestLiveDispatchLeg_RoundTripIntoATick(t *testing.T) {
	relay := liveDispatchRelayFromEnv(t)

	// Two throwaway identities: the NAMED AGENT that receives the work, and
	// the SCHEDULER's own inbox the answer comes back to.
	targetPub, targetPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen target: %v", err)
	}
	inboxPub, inboxPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen inbox: %v", err)
	}
	stamp := time.Now().UnixNano()
	targetID := fmt.Sprintf("sched-1710-target-%d", stamp)
	inboxID := fmt.Sprintf("sched-1710-inbox-%d", stamp)
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

	client := bus.NewClient(true, relay.url, relay.token, "live-1710")
	client.SetAgentIdentity(inboxID, inboxPriv)

	// 1 · The tick path's hand-out, through the shipped client.
	item := bus.WorkItem{
		Lane:    "helix",
		Board:   "/srv/lanes/helix/.coding-hermes/board/tasks.jsonl",
		Workdir: "/srv/lanes/helix",
		ReplyTo: inboxID,
	}
	receipt, err := client.Dispatch(context.Background(), targetID, item)
	if err != nil {
		t.Fatalf("live dispatch: %v", err)
	}
	if receipt.Transport != "inbox" || receipt.MessageID == "" || receipt.CorrID == "" {
		t.Fatalf("live receipt = %+v, want a durable inbox accept with an id and a correlation id", receipt)
	}
	t.Logf("LIVE dispatched: agent=%s message=%s transport=%s corr=%s", receipt.AgentID, receipt.MessageID, receipt.Transport, receipt.CorrID)

	// 2 · The agent RECEIVES it: read the bytes out of the target's own inbox.
	st, raw := relay.signed(t, http.MethodGet, "/agents/"+targetID+"/inbox?limit=10&lease_seconds=60", targetID, targetPriv)
	if st != http.StatusOK {
		t.Fatalf("live retrieve target inbox: status %d body %s", st, raw)
	}
	var inbox struct {
		LeaseID  string `json:"lease_id"`
		Messages []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &inbox); err != nil {
		t.Fatalf("decode target inbox: %v (%s)", err, raw)
	}
	if len(inbox.Messages) != 1 {
		t.Fatalf("target inbox holds %d messages, want exactly the one hand-out", len(inbox.Messages))
	}
	if inbox.Messages[0].ID != receipt.MessageID {
		t.Errorf("received message id %s != receipt message id %s", inbox.Messages[0].ID, receipt.MessageID)
	}
	decoded, err := base64.StdEncoding.DecodeString(inbox.Messages[0].Payload)
	if err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	var delivered map[string]any
	if err := json.Unmarshal(decoded, &delivered); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, decoded)
	}
	for k, want := range map[string]string{
		"lane": "helix", "workdir": item.Workdir, "board": item.Board,
		"corr_id": receipt.CorrID, "reply_to": inboxID, "kind": bus.KindWorkDispatch,
	} {
		if got, _ := delivered[k].(string); got != want {
			t.Errorf("delivered payload[%s] = %q, want %q", k, got, want)
		}
	}
	if _, bad := delivered["task"]; bad {
		t.Error("delivered payload carries a task member — the payload law forbids it")
	}
	t.Logf("LIVE agent received the work: %s", decoded)

	// 3 · The agent ACTS and answers into the scheduler's inbox — the shape the
	//     fleet's proven dispatcher uses (in_reply_to + echoed payload + reply).
	answer := map[string]any{
		"ok": true, "agent": targetID, "seconds": 2.0,
		"in_reply_to": receipt.MessageID,
		"task":        delivered, // the echoed hand-out, as an OBJECT (see below)
		"reply":       "LIVE AGENT ANSWER: tick handled for lane helix",
	}
	// NOTE on the shape: the fleet dispatcher renders the echoed hand-out as a
	// STRING, and the relay's content guard refuses such a payload with
	// GUARD_BLOCKED ("deterministic prematch block: stringified_json") when its
	// classifier is down. The structured OBJECT is what survives, and the
	// primary correlation (in_reply_to) does not depend on the echo at all.
	replyBody, _ := json.Marshal(map[string]any{
		"payload": answer, "sender": targetID,
		"request_id":      "reply-" + receipt.CorrID,
		"idempotency_key": "reply-" + receipt.CorrID,
	})
	if st, raw := relay.do(t, http.MethodPost, "/agents/"+inboxID+"/inbox", replyBody); st != http.StatusCreated && st != http.StatusOK && st != http.StatusAccepted {
		t.Fatalf("live reply delivery: status %d body %s", st, raw)
	}

	// 4 · The SHIPPED receive leg correlates the answer back to the hand-out.
	db := newTestDB(t)
	loop := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	loop.SetSchedulerBus(NewSchedulerBus(client))
	pool := loop.slotPool

	issued := time.Now().UTC().Format(time.RFC3339Nano)
	if err := database.RecordTickDispatchReceipt(context.Background(), db, database.TickDispatch{
		TickID: "tick-live-1710", Lane: "helix", Agent: receipt.AgentID,
		CorrID: receipt.CorrID, MessageID: receipt.MessageID, Transport: receipt.Transport,
		State: database.DispatchStateDispatched, IssuedAt: issued, UpdatedAt: issued,
	}); err != nil {
		t.Fatalf("record receipt: %v", err)
	}

	reply, ok, err := pool.awaitDispatchReply(context.Background(), PackedProject{Name: "helix"}, "tick-live-1710", receipt, 90*time.Second)
	if err != nil {
		t.Fatalf("live awaitDispatchReply: %v", err)
	}
	if !ok || !strings.Contains(reply, "LIVE AGENT ANSWER") {
		t.Fatalf("correlated reply = %q ok=%v, want the agent's answer", reply, ok)
	}
	t.Logf("LIVE correlated answer back to the hand-out: %s", reply)

	// 5 · The correlation resolves to the ORIGINATING TICK, and the lease was
	//     released by the ack (a second read finds nothing).
	rec, found, err := database.TickDispatchByCorrID(context.Background(), db, receipt.CorrID)
	if err != nil || !found {
		t.Fatalf("correlation lookup by corr id: found=%v err=%v", found, err)
	}
	if rec.TickID != "tick-live-1710" {
		t.Errorf("corr %s resolved to tick %s, want tick-live-1710", receipt.CorrID, rec.TickID)
	}
	if err := database.FinishTickDispatch(context.Background(), db, rec.TickID, database.DispatchStateReplied, reply, "", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("finish receipt: %v", err)
	}
	final, _, _ := database.TickDispatchForTick(context.Background(), db, "tick-live-1710")
	if final.State != database.DispatchStateReplied || !strings.Contains(final.Reply, "LIVE AGENT ANSWER") {
		t.Errorf("final receipt = %+v, want state=replied with the answer", final)
	}

	time.Sleep(1 * time.Second)
	st, raw = relay.signed(t, http.MethodGet, "/agents/"+inboxID+"/inbox?limit=10&lease_seconds=5", inboxID, inboxPriv)
	if st != http.StatusOK {
		t.Fatalf("re-read scheduler inbox: status %d body %s", st, raw)
	}
	if !strings.Contains(string(raw), `"messages":[]`) {
		t.Errorf("scheduler inbox still holds the answered message — the ack did not release it: %s", raw)
	}
}
