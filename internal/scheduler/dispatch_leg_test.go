package scheduler

// SCHED-GAP-1710 — hermetic proof that a TICK hands its work to a named agent
// and that the agent's answer is correlated back to the originating tick.
//
// The relay is a live-in-process HTTP/WebSocket-free double that implements
// exactly the three shipped routes the leg uses:
//
//	POST /agents/{id}/inbox          → the hand-out (dispatch.go)
//	GET  /agents/{id}/inbox          → the agent RECEIVING the work, and the
//	                                   scheduler reading its answer (inbox.go)
//	POST /agents/{id}/inbox/ack      → the scheduler releasing the lease
//
// It verifies the ed25519 signature on the agent-scoped routes (so a leg that
// signed the wrong path fails here), records every hand-out, and plays the
// agent: when the scheduler first reads its inbox after a hand-out, the double
// enqueues the answer the agent "produced" — with the relay message id as
// in_reply_to and the original payload echoed as `task`, exactly the shape the
// fleet's proven dispatcher emits.
//
// The GATEWAY is a separate recorder server: every test asserts it saw ZERO
// requests, which is how "no silent fallback to the shared gateway" is proven
// rather than asserted.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

type fakeMsg struct {
	id      string
	sender  string
	payload []byte
}

type fakeDispatch struct {
	agent          string
	payload        map[string]any
	sender         string
	requestID      string
	idempotencyKey string
	messageID      string
}

// fakeRelay is the double described above.
type fakeRelay struct {
	t *testing.T

	schedulerInbox string
	targetAgent    string
	replyText      string
	agentOK        *bool // nil = no verdict member (a plain answer)
	holdReply      bool  // leave accepted work unanswered to exercise cancel/timeout exits
	// preseed (SCHED-GAP-16984): foreign messages dropped into the
	// scheduler's inbox once, before the answer materialises, so the first
	// retrieve leases them together.
	preseed   []fakeMsg
	preseeded bool

	pub ed25519.PublicKey

	mu         sync.Mutex
	inbox      map[string][]fakeMsg
	dispatches []fakeDispatch
	unknown    map[string]bool
	acks       []string
	leases     int
	retrieves  int
	replied    bool
}

func newFakeRelay(t *testing.T, schedulerInbox, targetAgent, replyText string) *fakeRelay {
	return &fakeRelay{
		t: t, schedulerInbox: schedulerInbox, targetAgent: targetAgent, replyText: replyText,
		inbox: map[string][]fakeMsg{}, unknown: map[string]bool{},
	}
}

func (f *fakeRelay) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/inbox"):
			agent := strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/inbox")
			f.handleDispatch(agent, r, w)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/inbox"):
			agent := strings.TrimSuffix(strings.TrimPrefix(path, "/agents/"), "/inbox")
			f.handleRetrieve(agent, r, w)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/inbox/ack"):
			f.handleAck(r, w)
		default:
			http.NotFound(w, r)
		}
	})
}

func (f *fakeRelay) handleDispatch(agent string, r *http.Request, w http.ResponseWriter) {
	var body struct {
		Payload        map[string]any `json:"payload"`
		Sender         string         `json:"sender"`
		RequestID      string         `json:"request_id"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("dispatch body: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unknown[agent] {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown agent"}`))
		return
	}
	mid := fmt.Sprintf("m-%d", len(f.dispatches)+1)
	f.dispatches = append(f.dispatches, fakeDispatch{
		agent: agent, payload: body.Payload, sender: body.Sender,
		requestID: body.RequestID, idempotencyKey: body.IdempotencyKey, messageID: mid,
	})
	f.inbox[agent] = append(f.inbox[agent], fakeMsg{
		id: mid, sender: body.Sender, payload: mustJSON(f.t, body.Payload),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": mid, "transport": "inbox", "status": "queued"})
}

func (f *fakeRelay) handleRetrieve(agent string, r *http.Request, w http.ResponseWriter) {
	if err := f.verifySignature(r); err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"` + err.Error() + `"}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases++
	if agent == f.schedulerInbox {
		f.retrieves++
		// SCHED-GAP-16984: any preseeded foreign messages land first, so the
		// first batch leases them next to the answer.
		if !f.preseeded && len(f.preseed) > 0 {
			f.inbox[agent] = append(f.inbox[agent], f.preseed...)
			f.preseeded = true
		}
		// The agent has RECEIVED the work (the hand-out is in its inbox) and
		// now answers: the scheduler's first read of its own inbox is when the
		// answer materialises, which is exactly the ordering the live fleet
		// has (dispatch → poller picks it up → agent runs → reply).
		if !f.holdReply && !f.replied && len(f.dispatches) > 0 {
			d := f.dispatches[len(f.dispatches)-1]
			answer := map[string]any{
				"in_reply_to": d.messageID,
				"agent":       d.agent,
				"seconds":     1.5,
				"task":        string(mustJSON(f.t, d.payload)),
				"reply":       f.replyText,
			}
			if f.agentOK != nil {
				answer["ok"] = *f.agentOK
			}
			f.inbox[agent] = append(f.inbox[agent], fakeMsg{
				id: "reply-1", sender: d.agent, payload: mustJSON(f.t, answer),
			})
			f.replied = true
		}
	}
	msgs := f.inbox[agent]
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id": m.id, "agent_id": agent, "sender": m.sender,
			"payload": base64.StdEncoding.EncodeToString(m.payload),
		})
	}
	f.inbox[agent] = nil // lease them; only the ack releases them
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"lease_id": fmt.Sprintf("lease-%d", f.leases), "messages": out,
	})
}

func (f *fakeRelay) handleAck(r *http.Request, w http.ResponseWriter) {
	var body struct {
		LeaseID    string   `json:"lease_id"`
		MessageIDs []string `json:"message_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("ack body: %v", err)
	}
	f.mu.Lock()
	f.acks = append(f.acks, body.MessageIDs...)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// verifySignature enforces the documented agent-tier contract; the public key
// is pinned in pubKeyHex below.
func (f *fakeRelay) verifySignature(r *http.Request) error {
	ts := r.Header.Get("X-Agent-Ts")
	sig, err := hex.DecodeString(r.Header.Get("X-Agent-Sig"))
	if err != nil {
		return fmt.Errorf("bad sig header")
	}
	signed := r.Method + "\n" + r.URL.Path + "\n" + ts
	if !ed25519.Verify(f.pubKey(), []byte(signed), sig) {
		return fmt.Errorf("signature does not verify")
	}
	return nil
}

func (f *fakeRelay) pubKey() ed25519.PublicKey { return f.pub }

func (f *fakeRelay) counts() (dispatches, retrieves int, acks []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatches), f.retrieves, append([]string(nil), f.acks...)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// newDispatchTestRig wires a fake relay, a bus client with an inbox identity,
// a scheduler loop owning the slot pool, a target FILE for lane "helix", and a
// gateway recorder that must stay untouched.
func newDispatchTestRig(t *testing.T, relay *fakeRelay, withIdentity bool) (*fakeRelay, *SlotPool, *httptest.Server, *int64, ed25519.PrivateKey) {
	t.Helper()
	server := httptest.NewServer(relay.handler())
	t.Cleanup(server.Close)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	relay.pub = pub

	client := bus.NewClient(true, server.URL, "", "test-sched")
	if withIdentity {
		client.SetAgentIdentity(relay.schedulerInbox, priv)
	}

	var gatewayHits int64
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&gatewayHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gw.Close)

	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "helix")
	targetFile := writeDispatchTargets(t, `{"lane":"helix","agent":"`+relay.targetAgent+`","workdir":"/agent/helix"}`+"\n")
	t.Setenv(EnvDispatchTargets, targetFile)

	loop := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	loop.SetSchedulerBus(NewSchedulerBus(client))
	pool := loop.slotPool
	pool.spawner.SetGatewayClient(NewGatewayClient(gw.URL, "k", 5*time.Second))
	pool.spawner.SetNoExecFallback(true)
	return relay, pool, gw, &gatewayHits, priv
}

func writeDispatchTargets(t *testing.T, lines string) string {
	t.Helper()
	path := t.TempDir() + "/dispatch-targets.jsonl"
	if err := writeFileHelper(path, lines); err != nil {
		t.Fatalf("write dispatch targets: %v", err)
	}
	return path
}

func writeFileHelper(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// TestDispatchLeg_HandsWorkToNamedAgentAndCorrelatesTheAnswer is the card's
// acceptance in one test: a tick hands ONE unit of work to ONE named agent
// carrying lane + board/workdir reference + correlation id (and never a task
// id), the agent RECEIVES it out of its own inbox, answers, and the answer is
// correlated back to the originating tick — which completes, with no request
// ever reaching the shared gateway.
func TestDispatchLeg_HandsWorkToNamedAgentAndCorrelatesTheAnswer(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "AGENT ANSWER: shipped the fix")
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, true)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "completed" {
		t.Fatalf("tick status = %q (error %q), want completed via the agent's answer", row.Status, row.Err)
	}

	// 1 · The agent RECEIVED the work: the hand-out is in its own inbox and
	//     carries exactly the payload law.
	got, retrieves, acks := relay.counts()
	if got != 1 {
		t.Fatalf("hand-outs = %d, want exactly 1", got)
	}
	if retrieves == 0 {
		t.Fatal("the scheduler never read its inbox: the answer could not have been correlated")
	}
	d := relay.dispatches[0]
	if d.agent != "helix-agent" {
		t.Errorf("hand-out agent = %q, want helix-agent (the NAMED agent)", d.agent)
	}
	if d.payload["kind"] != bus.KindWorkDispatch || d.payload["lane"] != "helix" {
		t.Errorf("hand-out payload = %v, want kind=work.dispatch lane=helix", d.payload)
	}
	if d.payload["workdir"] != "/agent/helix" {
		t.Errorf("hand-out workdir = %v, want the target's reference", d.payload["workdir"])
	}
	if d.payload["reply_to"] != "test-sched-inbox" {
		t.Errorf("hand-out reply_to = %v, want the scheduler's inbox", d.payload["reply_to"])
	}
	if _, bad := d.payload["task"]; bad {
		t.Error("hand-out payload carries a TASK member — the scheduler picks the LANE, the foreman picks the TASK")
	}
	corr, _ := d.payload["corr_id"].(string)
	if corr == "" || corr != d.requestID || corr != d.idempotencyKey {
		t.Errorf("corr_id %q must ride as request_id (%q) and idempotency_key (%q)", corr, d.requestID, d.idempotencyKey)
	}

	// 2 · The tick recorded the receipt against itself: the agent USED, the
	//     correlation id, the relay's message id, and the answer.
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, ok, err)
	}
	if rec.State != database.DispatchStateReplied {
		t.Errorf("receipt state = %q, want replied", rec.State)
	}
	if rec.Agent != "helix-agent" || rec.CorrID == "" || rec.MessageID == "" || rec.Transport != "inbox" {
		t.Errorf("receipt = %+v, want the agent used + corr + relay message id + transport", rec)
	}
	if !strings.Contains(rec.Reply, "AGENT ANSWER") {
		t.Errorf("receipt reply = %q, want the agent's answer", rec.Reply)
	}
	// 3 · The answer reached the tick's own row too (the delivered report).
	tickErr := queryTickError(t, db, row.ID)
	if tickErr != "" {
		t.Errorf("completed tick carries error %q, want empty", tickErr)
	}
	if len(acks) == 0 {
		t.Error("the correlated answer was never acked — the lease would keep hiding it")
	}

	// 4 · NO FALLBACK: not one request reached the shared gateway.
	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests — a remote lane must never fall back to the shared gateway", n)
	}
}

// TestDispatchLeg_UnreachableAgentFailsLoudlyWithNoFallback — an unknown agent
// is a NAMED error, the attempt is recorded, the tick FAILS, and nothing is
// re-pointed at the gateway.
func TestDispatchLeg_UnreachableAgentFailsLoudlyWithNoFallback(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "gone-agent", "")
	relay.unknown["gone-agent"] = true
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, true)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "failed" {
		t.Fatalf("tick status = %q, want failed (the hand-out was refused)", row.Status)
	}
	if !strings.Contains(row.Err, "target unknown") {
		t.Errorf("tick error = %q, want the named unknown-target wording", row.Err)
	}
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row missing: ok=%v err=%v", ok, err)
	}
	if rec.State != database.DispatchStateFailed || !strings.Contains(rec.Error, "target unknown") {
		t.Errorf("receipt = %+v, want state=failed carrying the refusal", rec)
	}
	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests after a refused dispatch — silent fallback", n)
	}
}

// TestDispatchLeg_RefusesWithoutAReplyIdentity — a remote lane and no inbox
// identity means the answer could never be correlated, so NOTHING is sent at
// all: the tick fails loudly before the hand-out, rather than dispatching work
// whose answer would be lost.
func TestDispatchLeg_RefusesWithoutAReplyIdentity(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "answer")
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, false /* no identity */)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "failed" {
		t.Fatalf("tick status = %q, want failed", row.Status)
	}
	if !strings.Contains(row.Err, "no inbox identity") {
		t.Errorf("tick error = %q, want the named missing-identity reason", row.Err)
	}
	if n, _, _ := relay.counts(); n != 0 {
		t.Errorf("hand-outs = %d with no reply identity, want 0 (nothing may be dispatched blind)", n)
	}
	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests, want 0", n)
	}
}

// TestDispatchLeg_AgentVerdictOfFailureFailsTheTick — the answering agent can
// say the work failed; its verdict is carried into the tick instead of being
// laundered into a completion.
func TestDispatchLeg_AgentVerdictOfFailureFailsTheTick(t *testing.T) {
	no := false
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "could not finish: repo missing")
	relay.agentOK = &no
	_, pool, _, _, _ := newDispatchTestRig(t, relay, true)

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "failed" {
		t.Fatalf("tick status = %q, want failed (the agent reported ok=false)", row.Status)
	}
	if !strings.Contains(row.Err, "could not finish") {
		t.Errorf("tick error = %q, want the agent's own failure text", row.Err)
	}
}

// TestCorrelateDispatchReply_IdentityNotArrival — only a message that NAMES our
// hand-out is an answer to it. Everything else stays with its owner.
func TestCorrelateDispatchReply_IdentityNotArrival(t *testing.T) {
	receipt := bus.DispatchReceipt{AgentID: "a", CorrID: "q-sched-1-9", MessageID: "m-7"}
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"in_reply_to the relay message id", `{"in_reply_to":"m-7","reply":"ok"}`, true},
		{"corr_id echoed directly", `{"corr_id":"q-sched-1-9","reply":"ok"}`, true},
		{"original payload echoed as task", `{"in_reply_to":"m-9","task":"{\"kind\":\"work.dispatch\",\"corr_id\":\"q-sched-1-9\"}","reply":"ok"}`, true},
		{"original payload echoed as a task OBJECT", `{"in_reply_to":"m-8","task":{"kind":"work.dispatch","corr_id":"q-sched-1-9"},"reply":"ok"}`, true},
		{"foreign message", `{"in_reply_to":"m-8","reply":"someone else's"}`, false},
		{"foreign task echo", `{"in_reply_to":"m-9","task":"{\"corr_id\":\"q-other-1-1\"}"}`, false},
		{"unreadable payload", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := bus.InboxMessage{ID: "x", PayloadBase64: base64.StdEncoding.EncodeToString([]byte(tc.payload))}
			if _, got := correlateDispatchReply(msg, receipt); got != tc.want {
				t.Errorf("correlate = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDispatchLeg_NamesAndAcksUncorrelatableReply (SCHED-GAP-16984): one
// matching reply and one foreign reply share the first batch. The matching one
// completes the tick as before; the foreign one — which the pre-16984 leg
// would have left leased and silent until TTL — is named in the tick detail
// with the correlation key it claimed and the seen/correlated/uncorrelated
// counts, and is acked so the inbox cannot accumulate unclassifiable residue.
func TestDispatchLeg_NamesAndAcksUncorrelatableReply(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "AGENT ANSWER: shipped the fix")
	relay.preseed = []fakeMsg{{
		id:     "stray-1",
		sender: "helix-agent",
		// Claims a correlation id that matches nothing this scheduler has
		// handed out: the exact shape the live inbox parked five of.
		payload: mustJSON(t, map[string]any{"corr_id": "q-foreign-16984-1", "reply": "someone else's answer"}),
	}}
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, true)

	before := DispatchInboxCounters()

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status != "completed" {
		t.Fatalf("tick status = %q (error %q), want completed via the matching reply", row.Status, row.Err)
	}

	// 1 · The matching one completed the tick AND both messages left the
	//     lease: the matched answer (as before) and the stray (the new
	//     settle path).
	_, _, acks := relay.counts()
	if !slicesContain(acks, "reply-1") {
		t.Errorf("acks = %v, want the matched reply acked", acks)
	}
	if !slicesContain(acks, "stray-1") {
		t.Errorf("acks = %v, want the uncorrelatable stray ACKED (not parked to TTL)", acks)
	}

	// 2 · The tick detail names the stray: counts, its claimed key, and how
	//     it left the lease — appended after the byte-unchanged answer.
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, ok, err)
	}
	if rec.State != database.DispatchStateReplied {
		t.Errorf("receipt state = %q, want replied", rec.State)
	}
	for _, want := range []string{
		"AGENT ANSWER: shipped the fix",
		"[dispatch-inbox] seen=2 correlated=1 uncorrelated=1 keys=q-foreign-16984-1 acked=1 deferred=0",
	} {
		if !strings.Contains(rec.Reply, want) {
			t.Errorf("receipt reply = %q, want it to carry %q", rec.Reply, want)
		}
	}

	// 3 · The counters moved by exactly the batch: seen=2, correlated=1,
	//     uncorrelated=1, acked=1, and no residue belief left behind.
	after := DispatchInboxCounters()
	if got := after.SeenTotal - before.SeenTotal; got != 2 {
		t.Errorf("seen_total delta = %d, want 2", got)
	}
	if got := after.CorrelatedTotal - before.CorrelatedTotal; got != 1 {
		t.Errorf("correlated_total delta = %d, want 1", got)
	}
	if got := after.UncorrelatedTotal - before.UncorrelatedTotal; got != 1 {
		t.Errorf("uncorrelated_total delta = %d, want 1", got)
	}
	if got := after.AckedTotal - before.AckedTotal; got != 1 {
		t.Errorf("acked_total delta = %d, want 1", got)
	}
	if got := after.DeferredTotal - before.DeferredTotal; got != 0 {
		t.Errorf("deferred_total delta = %d, want 0 (the stray was acked, not deferred)", got)
	}
	if got := after.UnackedCurrent - before.UnackedCurrent; got != 0 {
		t.Errorf("unacked_current delta = %d, want 0 (an acked stray is not residue)", got)
	}

	// 4 · NO FALLBACK: not one request reached the shared gateway.
	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests — a remote lane must never fall back to the shared gateway", n)
	}
}

// TestDispatchLeg_AllUncorrelatedBatchNeverCompletesTheTick (SCHED-GAP-16984
// negative control): a batch in which EVERY message fails to correlate must
// not complete the tick, must report uncorrelated == seen with every claimed
// key named, and must still settle the messages (acked here: nothing
// outstanding claims them).
func TestDispatchLeg_AllUncorrelatedBatchNeverCompletesTheTick(t *testing.T) {
	relay := newFakeRelay(t, "test-sched-inbox", "helix-agent", "irrelevant: never delivered")
	relay.holdReply = true // no correlatable answer ever materialises
	relay.preseed = []fakeMsg{
		{id: "stray-1", sender: "helix-agent", payload: mustJSON(t, map[string]any{"corr_id": "q-foreign-a-16984", "reply": "no one's answer"})},
		{id: "stray-2", sender: "helix-agent", payload: mustJSON(t, map[string]any{"corr_id": "q-foreign-b-16984", "reply": "still no one's answer"})},
	}
	_, pool, _, gatewayHits, _ := newDispatchTestRig(t, relay, true)
	// A short await budget: the deadline is the verdict under test.
	pool.owner.SetTickTimeout(2 * time.Second)

	before := DispatchInboxCounters()

	db := pool.spawner.db
	pool.Spawn(PackedProject{Name: "helix", Workdir: "/tmp/helix"}, time.Now(), true, db)

	row := waitForTerminalTick(t, db, "helix", 30*time.Second)
	if row.Status == "completed" {
		t.Fatal("a batch with NO correlatable reply completed the tick — residue must never close it")
	}

	// The receipt expired AND records the whole batch as uncorrelated: the
	// counts, every claimed key, and the acks that drained them.
	rec, ok, err := database.TickDispatchForTick(context.Background(), db, row.ID)
	if err != nil || !ok {
		t.Fatalf("tick_dispatch row for %s: ok=%v err=%v", row.ID, ok, err)
	}
	if rec.State != database.DispatchStateExpired {
		t.Errorf("receipt state = %q, want expired (no correlated reply arrived)", rec.State)
	}
	want := "[dispatch-inbox] seen=2 correlated=0 uncorrelated=2 keys=q-foreign-a-16984,q-foreign-b-16984 acked=2 deferred=0"
	if !strings.Contains(rec.Reply, want) {
		t.Errorf("receipt reply = %q, want it to carry %q (uncorrelated == seen, keys named)", rec.Reply, want)
	}

	_, _, acks := relay.counts()
	if !slicesContain(acks, "stray-1") || !slicesContain(acks, "stray-2") {
		t.Errorf("acks = %v, want both strays acked instead of parked to TTL", acks)
	}

	after := DispatchInboxCounters()
	if got := after.UncorrelatedTotal - before.UncorrelatedTotal; got != 2 {
		t.Errorf("uncorrelated_total delta = %d, want 2", got)
	}
	if got := after.SeenTotal - before.SeenTotal; got != after.UncorrelatedTotal-before.UncorrelatedTotal {
		t.Errorf("seen delta %d != uncorrelated delta %d — a batch that ALL fails to correlate must report uncorrelated == seen",
			after.SeenTotal-before.SeenTotal, after.UncorrelatedTotal-before.UncorrelatedTotal)
	}
	if got := after.AckedTotal - before.AckedTotal; got != 2 {
		t.Errorf("acked_total delta = %d, want 2", got)
	}

	if n := atomic.LoadInt64(gatewayHits); n != 0 {
		t.Errorf("gateway saw %d requests, want 0", n)
	}
}

// TestDispatchReplyOwnedElsewhere_GuardsAnInFlightHandOut (SCHED-GAP-16984):
// the guard that keeps residue-acking from consuming a SIBLING tick's live
// answer. A message whose claimed identity names a tick_dispatch row still in
// state=dispatched is owned elsewhere; once that hand-out is answered (or for
// strangers) it is not; without a db the answer must fail closed.
func TestDispatchReplyOwnedElsewhere_GuardsAnInFlightHandOut(t *testing.T) {
	db := newTestDB(t)
	issued := time.Now().UTC().Format(time.RFC3339Nano)
	rec := database.TickDispatch{
		TickID: "tick-owned-16984", Lane: "helix", Agent: "helix-agent",
		CorrID: "q-owned-16984", MessageID: "m-owned-16984", Transport: "inbox",
		State: database.DispatchStateDispatched, IssuedAt: issued, UpdatedAt: issued,
	}
	if err := database.RecordTickDispatchReceipt(context.Background(), db, rec); err != nil {
		t.Fatalf("record receipt: %v", err)
	}
	ctx := context.Background()

	owned, err := dispatchReplyOwnedElsewhere(ctx, db, "q-owned-16984", "")
	if err != nil || !owned {
		t.Errorf("corr_id of an in-flight hand-out: owned=%v err=%v, want true", owned, err)
	}
	owned, err = dispatchReplyOwnedElsewhere(ctx, db, "", "m-owned-16984")
	if err != nil || !owned {
		t.Errorf("message id of an in-flight hand-out: owned=%v err=%v, want true", owned, err)
	}
	owned, err = dispatchReplyOwnedElsewhere(ctx, db, "q-stranger", "m-stranger")
	if err != nil || owned {
		t.Errorf("a stranger's identity: owned=%v err=%v, want false", owned, err)
	}

	// Once the hand-out is answered it is no longer outstanding.
	rec.State = database.DispatchStateReplied
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := database.RecordTickDispatchReceipt(context.Background(), db, rec); err != nil {
		t.Fatalf("update receipt: %v", err)
	}
	owned, err = dispatchReplyOwnedElsewhere(ctx, db, "q-owned-16984", "")
	if err != nil || owned {
		t.Errorf("answered hand-out: owned=%v err=%v, want false (no longer outstanding)", owned, err)
	}

	// Fail-closed: without a db the question is unanswerable and must say so
	// (the settle path defers rather than consuming).
	if _, err := dispatchReplyOwnedElsewhere(ctx, nil, "q-owned-16984", ""); err == nil {
		t.Error("nil db must fail closed with an error, not pretend ownership is knowable")
	}
}

// slicesContain reports whether vs contains v (test helper for the ack lists).
func slicesContain(vs []string, v string) bool {
	for _, s := range vs {
		if s == v {
			return true
		}
	}
	return false
}

// tickRow is the slice of a ticks row these tests assert on.
type tickRow struct {
	ID     string
	Status string
	Err    string
}

func waitForTerminalTick(t *testing.T, db *sql.DB, project string, budget time.Duration) tickRow {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		var row tickRow
		var status, errText string
		q := `SELECT id, status, COALESCE(error,'') FROM ticks WHERE project_name = ? ORDER BY created_at DESC, id DESC LIMIT 1`
		if err := db.QueryRow(q, project).Scan(&row.ID, &status, &errText); err == nil {
			row.Status, row.Err = status, errText
			last = status
			switch status {
			case "completed", "failed", "timeout", "deferred":
				return row
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("tick for %s never reached a terminal status (last seen %q)", project, last)
	return tickRow{}
}

func queryTickError(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var errText string
	if err := db.QueryRow(`SELECT COALESCE(error,'') FROM ticks WHERE id = ?`, tickID).Scan(&errText); err != nil {
		t.Fatalf("read tick error: %v", err)
	}
	return errText
}
