package bus

// SCHED-GAP-1710 — hermetic proof of the INBOX receive leg (internal/bus/
// inbox.go): the agent-scoped retrieve/ack surface the scheduler reads its
// dispatch answers from.
//
// The relay requires the mailbox owner's ed25519 signature over
// "METHOD\n<path>\n<unix-seconds>" with the QUERY STRING EXCLUDED. These tests
// verify that contract against a live-in-process verifier, so a client that
// signed the query (or the wrong path, or the wrong method) fails here rather
// than silently receiving 401s against the real relay.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testKeyPair writes a PKCS#8 PEM key the way `crier keygen` does and returns
// the parsed private key plus its file path.
func testKeyPair(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent.key")
	blk := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, blk, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return priv, path
}

// TestLoadAgentKeyFileReadsKeygenOutput — the fleet's key files are PKCS#8 PEM.
func TestLoadAgentKeyFileReadsKeygenOutput(t *testing.T) {
	priv, path := testKeyPair(t)
	got, err := LoadAgentKeyFile(path)
	if err != nil {
		t.Fatalf("LoadAgentKeyFile: %v", err)
	}
	if !got.Equal(priv) {
		t.Fatal("loaded key differs from the one written")
	}
	if _, err := LoadAgentKeyFile(filepath.Join(t.TempDir(), "absent.key")); err == nil {
		t.Error("load of a missing key file returned nil error")
	}
}

// TestInboxRoutesRequireIdentity — an unconfigured receive leg must refuse
// locally, without any I/O: an unconfigured inbox must never look empty.
func TestInboxRoutesRequireIdentity(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt64(&hits, 1) }))
	defer srv.Close()

	c := NewClient(true, srv.URL, "tok", "sched")
	if c.InboxEnabled() {
		t.Fatal("InboxEnabled = true with no identity")
	}
	if _, err := c.InboxRetrieve(context.Background(), 5, 30); err == nil || !strings.Contains(err.Error(), "no agent inbox identity") {
		t.Errorf("InboxRetrieve without identity = %v, want ErrAgentIdentityMissing", err)
	}
	if err := c.InboxAck(context.Background(), "lease", "msg"); err == nil || !strings.Contains(err.Error(), "no agent inbox identity") {
		t.Errorf("InboxAck without identity = %v, want ErrAgentIdentityMissing", err)
	}
	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Errorf("%d requests reached the relay without an identity, want 0", got)
	}
}

// TestInboxRetrieveSignsTheDocumentedContract — the server verifies the
// signature exactly as the relay documents it (path without the query string)
// and answers a lease; the client returns the decoded lease.
func TestInboxRetrieveSignsTheDocumentedContract(t *testing.T) {
	priv, _ := testKeyPair(t)
	pub := priv.Public().(ed25519.PublicKey)

	payload := map[string]any{"kind": "work.reply", "in_reply_to": "m-1", "reply": "done"}
	raw, _ := json.Marshal(payload)

	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts := r.Header.Get("X-Agent-Ts")
		sig, err := hex.DecodeString(r.Header.Get("X-Agent-Sig"))
		if err != nil {
			t.Errorf("sig header is not hex: %v", err)
		}
		if got := r.Header.Get("X-Agent-ID"); got != "sched-inbox" {
			t.Errorf("X-Agent-ID = %q", got)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer")
		}
		// The signed path EXCLUDES the query string.
		signed := r.Method + "\n" + r.URL.Path + "\n" + ts
		if !ed25519.Verify(pub, []byte(signed), sig) {
			t.Errorf("signature does not verify over %q", signed)
		}
		sawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"lease_id": "lease-1",
			"messages": []map[string]any{{
				"id": "m-1", "sender": "dispatch-1",
				"payload": base64.StdEncoding.EncodeToString(raw),
			}},
		})
	}))
	defer srv.Close()

	c := NewClient(true, srv.URL, "tok", "sched")
	c.SetAgentIdentity("sched-inbox", priv)
	if !c.InboxEnabled() {
		t.Fatal("InboxEnabled = false after SetAgentIdentity")
	}
	lease, err := c.InboxRetrieve(context.Background(), 5, 30)
	if err != nil {
		t.Fatalf("InboxRetrieve: %v", err)
	}
	if !strings.Contains(sawQuery, "limit=5") || !strings.Contains(sawQuery, "lease_seconds=30") {
		t.Errorf("query = %q, want limit+lease_seconds", sawQuery)
	}
	if lease.LeaseID != "lease-1" || len(lease.Messages) != 1 {
		t.Fatalf("lease = %+v, want one message under lease-1", lease)
	}
	got, err := lease.Messages[0].Payload()
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got["in_reply_to"] != "m-1" || got["reply"] != "done" {
		t.Errorf("decoded payload = %v", got)
	}
}

// TestInboxAckPostsLeaseAndMessages — the ack body is the documented shape,
// and a refusal is a NAMED error (the message stays leased; the operator sees
// why).
func TestInboxAckPostsLeaseAndMessages(t *testing.T) {
	priv, _ := testKeyPair(t)

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/inbox/ack") {
			t.Errorf("ack path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(true, srv.URL, "", "sched")
	c.SetAgentIdentity("sched-inbox", priv)
	if err := c.InboxAck(context.Background(), "lease-1", "m-1", "m-2"); err != nil {
		t.Fatalf("InboxAck: %v", err)
	}
	if got["lease_id"] != "lease-1" {
		t.Errorf("ack lease_id = %v", got["lease_id"])
	}
	ids, _ := got["message_ids"].([]any)
	if len(ids) != 2 || ids[0] != "m-1" || ids[1] != "m-2" {
		t.Errorf("ack message_ids = %v", got["message_ids"])
	}

	// A refused ack is named, not silent.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not the mailbox owner"}`))
	}))
	defer bad.Close()
	c2 := NewClient(true, bad.URL, "", "sched")
	c2.SetAgentIdentity("sched-inbox", priv)
	if err := c2.InboxAck(context.Background(), "lease-1", "m-1"); err == nil || !strings.Contains(err.Error(), "inbox refused") {
		t.Errorf("refused ack = %v, want the named ErrInboxRefused wording", err)
	}
}

// TestDispatchPayloadCarriesReplyTo — the hand-out names the address the ANSWER
// must be sent to, and — the payload law — still carries NO task member.
func TestDispatchPayloadCarriesReplyTo(t *testing.T) {
	_, body, corr, err := BuildDispatch("http://relay", "sched", "helix",
		WorkItem{Lane: "helix", Workdir: "/w", ReplyTo: "sched-inbox"}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("BuildDispatch: %v", err)
	}
	var req struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Payload["reply_to"] != "sched-inbox" {
		t.Errorf("payload.reply_to = %v, want sched-inbox", req.Payload["reply_to"])
	}
	if req.Payload["corr_id"] != corr {
		t.Errorf("payload.corr_id = %v, want %s", req.Payload["corr_id"], corr)
	}
	assertNoTaskMember(t, body)

	// Absent ReplyTo keeps the historical payload (no empty member).
	_, body2, _, err := BuildDispatch("http://relay", "sched", "helix",
		WorkItem{Lane: "helix", Workdir: "/w"}, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("BuildDispatch: %v", err)
	}
	if strings.Contains(string(body2), "reply_to") {
		t.Errorf("payload carries reply_to with an empty ReplyTo: %s", body2)
	}
	_ = strconv.Itoa(0)
}
