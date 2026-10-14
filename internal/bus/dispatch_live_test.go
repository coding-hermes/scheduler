package bus

// LIVE dispatch proof (SCHED-GAP-1665, the dispatch leg).
//
// This file is the reproducible evidence that the verb is real: it speaks
// to a RUNNING Crier relay, hands a work item to a real agent inbox, reads
// the delivered bytes back out of that inbox, and proves the relay's
// idempotency key makes a retried dispatch one message rather than two.
//
// It is SKIPPED unless a live relay is named, so `go test ./...` stays
// hermetic:
//
//	CRIER_LIVE_URL=http://127.0.0.1:8767 \
//	CRIER_LIVE_TOKEN=<relay bearer> \
//	go test ./internal/bus/ -run TestLiveDispatch -v -count=1
//
// The test registers a throwaway agent (id prefixed sched-dispatch-proof-),
// uses it, and unregisters it in cleanup — it never touches a real project
// agent's inbox.

import (
	"bytes"
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
	"strconv"
	"strings"
	"testing"
	"time"
)

type liveRelay struct {
	url   string
	token string
	http  *http.Client
}

func liveRelayFromEnv(t *testing.T) *liveRelay {
	t.Helper()
	url := strings.TrimRight(os.Getenv("CRIER_LIVE_URL"), "/")
	if url == "" {
		t.Skip("CRIER_LIVE_URL not set: live dispatch proof skipped (set it to a running relay to run this test)")
	}
	return &liveRelay{
		url:   url,
		token: os.Getenv("CRIER_LIVE_TOKEN"),
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (l *liveRelay) do(t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
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

// signedDo signs the agent-tier request the Crier contract documents:
// hex(ed25519(sign(priv, "METHOD\n<escaped path>\n<unix-seconds>"))), where
// the signed path EXCLUDES the query string (the request URL keeps it).
func (l *liveRelay) signedDo(t *testing.T, method, path, agentID string, priv ed25519.PrivateKey) (int, []byte) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
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

func TestLiveDispatchHandsAWorkItemToAnAgentInbox(t *testing.T) {
	relay := liveRelayFromEnv(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	agentID := fmt.Sprintf("sched-dispatch-proof-%d", time.Now().UnixNano())

	// 1 · Register the throwaway agent (the target must exist for a durable
	//     inbox to be created).
	regBody, _ := json.Marshal(map[string]any{
		"id":           agentID,
		"public_key":   hex.EncodeToString(pub),
		"capabilities": []string{"sched.dispatch.proof"},
	})
	status, raw := relay.do(t, http.MethodPost, "/agents", regBody)
	if status != http.StatusCreated {
		t.Fatalf("register probe agent: status %d body %s", status, raw)
	}
	t.Cleanup(func() {
		if st, body := relay.signedDo(t, http.MethodDelete, "/agents/"+agentID, agentID, priv); st != http.StatusNoContent {
			t.Logf("CLEANUP WARNING: probe agent %s not unregistered (status %d body %s)", agentID, st, body)
		}
	})

	// 2 · Dispatch a work item through the SHIPPED bus client.
	client := NewClient(true, relay.url, relay.token, "live-proof-sched")
	item := WorkItem{
		Lane:    "sched-dispatch-proof",
		Board:   "/home/kara/auger/.coding-hermes/board/tasks.jsonl",
		Workdir: "/home/kara/auger",
		CorrID:  fmt.Sprintf("q-live-proof-%d", time.Now().UnixNano()),
	}
	receipt, err := client.Dispatch(context.Background(), agentID, item)
	if err != nil {
		t.Fatalf("live Dispatch: %v", err)
	}
	if receipt.Transport != "inbox" {
		t.Fatalf("receipt transport = %q, want inbox (durable hand-out)", receipt.Transport)
	}
	if receipt.MessageID == "" || receipt.CorrID != item.CorrID {
		t.Fatalf("receipt = %+v, want a message id and the caller's correlation id", receipt)
	}
	t.Logf("live dispatch accepted: agent=%s message=%s transport=%s corr=%s",
		receipt.AgentID, receipt.MessageID, receipt.Transport, receipt.CorrID)

	// 3 · Retrying the SAME correlation id (the retry-after-ambiguous-timeout
	//     path) must be answered from the recorded accept: one key, one
	//     message, no duplicate job.
	retry, err := client.Dispatch(context.Background(), agentID, item)
	if err != nil {
		t.Fatalf("live Dispatch retry: %v", err)
	}
	if !retry.IdempotentReplay || retry.MessageID != receipt.MessageID {
		t.Fatalf("retry receipt = %+v, want an idempotent replay of %s", retry, receipt.MessageID)
	}

	// 4 · Read the delivered bytes back out of the agent's own inbox.
	st, raw := relay.signedDo(t, http.MethodGet, "/agents/"+agentID+"/inbox?limit=10&lease_seconds=30", agentID, priv)
	if st != http.StatusOK {
		t.Fatalf("signed retrieve: status %d body %s", st, raw)
	}
	var inbox struct {
		Messages []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
			Sender  string `json:"sender"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &inbox); err != nil {
		t.Fatalf("unmarshal retrieve: %v (%s)", err, raw)
	}
	if len(inbox.Messages) != 1 {
		t.Fatalf("inbox holds %d messages, want exactly 1 (the idempotency key must suppress the retry)", len(inbox.Messages))
	}
	msg := inbox.Messages[0]
	if msg.ID != receipt.MessageID {
		t.Errorf("retrieved message id %s != receipt message id %s", msg.ID, receipt.MessageID)
	}
	if msg.Sender != "scheduler-live-proof-sched" {
		t.Errorf("sender = %q, want scheduler-live-proof-sched", msg.Sender)
	}
	decoded, err := base64.StdEncoding.DecodeString(msg.Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v (%s)", err, msg.Payload)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v (%s)", err, decoded)
	}
	for k, want := range map[string]string{
		"kind":    KindWorkDispatch,
		"lane":    "sched-dispatch-proof",
		"board":   item.Board,
		"workdir": item.Workdir,
		"corr_id": item.CorrID,
	} {
		if got, _ := payload[k].(string); got != want {
			t.Errorf("delivered payload[%s] = %q, want %q", k, got, want)
		}
	}
	assertNoTaskMember(t, decoded)
	t.Logf("delivered payload round-tripped byte-for-byte: %s", decoded)

	// 5 · An unknown target is LOUD against the live relay (no silent
	//     fallback to any other transport).
	if _, err := client.Dispatch(context.Background(), agentID+"-absent", item); err == nil {
		t.Fatal("dispatch to an unregistered agent returned nil error")
	} else if !strings.Contains(err.Error(), "target unknown") {
		t.Fatalf("unknown-target error = %v, want the named ErrDispatchTargetUnknown wording", err)
	}
}
