package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitPositionalsKeepsFlagValuesOutOfThePositionals(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		positional []string
	}{
		{"flags before the agent", []string{"--lane", "auger", "agent-7"}, []string{"agent-7"}},
		{"flags after the agent", []string{"agent-7", "--lane", "auger"}, []string{"agent-7"}},
		{"equals form", []string{"agent-7", "--lane=auger"}, []string{"agent-7"}},
		{"bool flags take no value", []string{"agent-7", "--dry-run", "--json"}, []string{"agent-7"}},
		{"double dash ends flags", []string{"agent-7", "--", "--not-a-flag"}, []string{"agent-7", "--not-a-flag"}},
		{"two positionals are preserved", []string{"a", "b"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			positional, flags := splitPositionals(tc.argv)
			if strings.Join(positional, ",") != strings.Join(tc.positional, ",") {
				t.Fatalf("positional = %v, want %v", positional, tc.positional)
			}
			// Every flag token must survive for flag.Parse to consume.
			joined := strings.Join(flags, " ")
			for _, want := range []string{"--lane", "--dry-run"} {
				if strings.Contains(strings.Join(tc.argv, " "), want) && !strings.Contains(joined, want) {
					t.Errorf("flag %s was dropped from %v", want, flags)
				}
			}
		})
	}
}

func TestRunUsageErrorsExitTwoWithoutSending(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	cases := [][]string{
		{"agent-7"},                                                  // no work item
		{"agent-7", "--lane", "auger"},                               // lane but no board/workdir ref
		{"agent-7", "--board", "/b"},                                 // board but no lane
		{"--lane", "auger", "--board", "/b"},                         // no agent
		{"agent-7", "--lane", "auger", "--board", "/b", "extra-arg"}, // two positionals
	}
	for i, argv := range cases {
		var stdout, stderr bytes.Buffer
		code := run(argv, &stdout, &stderr)
		if code != 2 {
			t.Errorf("case %d %v: exit %d, want 2 (usage/config; nothing attempted)\nstdout=%s\nstderr=%s", i, argv, code, stdout.String(), stderr.String())
		}
	}
	if called {
		t.Fatal("a usage error reached a relay")
	}
}

func TestRunDryRunRendersTheWireBodyWithoutSending(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"agent-7", "--lane", "auger", "--board", "/b/tasks.jsonl", "--workdir", "/w",
		"--corr-id", "q-cli-1", "--url", srv.URL, "--scheduler-id", "sched-cli", "--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr=%s", code, stderr.String())
	}
	if called {
		t.Fatal("--dry-run sent a request")
	}
	out := stdout.String()
	if !strings.Contains(out, "POST "+srv.URL+"/agents/agent-7/inbox") {
		t.Errorf("dry run does not name the endpoint:\n%s", out)
	}
	if !strings.Contains(out, "q-cli-1") {
		t.Errorf("dry run does not carry the correlation id:\n%s", out)
	}
	body := out[strings.Index(out, "body: ")+len("body: "):]
	body = strings.TrimSpace(body)
	var req struct {
		Payload        map[string]any `json:"payload"`
		Sender         string         `json:"sender"`
		RequestID      string         `json:"request_id"`
		IdempotencyKey string         `json:"idempotency_key"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("dry-run body is not JSON: %v (%s)", err, body)
	}
	if req.Sender != "scheduler-sched-cli" {
		t.Errorf("sender = %q", req.Sender)
	}
	if req.RequestID != "q-cli-1" || req.IdempotencyKey != "q-cli-1" {
		t.Errorf("correlation carriers = %q / %q, want q-cli-1 on both", req.RequestID, req.IdempotencyKey)
	}
	if req.Payload["lane"] != "auger" || req.Payload["board"] != "/b/tasks.jsonl" || req.Payload["workdir"] != "/w" {
		t.Errorf("payload = %v", req.Payload)
	}
	if req.Payload["kind"] != "work.dispatch" {
		t.Errorf("payload.kind = %v", req.Payload["kind"])
	}
}

func TestRunAcceptedDispatchPrintsReceipt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/agent-7/inbox" {
			t.Errorf("deliver path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"msg-cli-1","transport":"inbox","expires_at":null}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"agent-7", "--lane", "auger", "--workdir", "/w",
		"--url", srv.URL, "--scheduler-id", "sched-cli",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"DISPATCHED", "agent=agent-7", "transport=inbox", "message=msg-cli-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("receipt line missing %q:\n%s", want, out)
		}
	}
}

func TestRunRefusedDispatchExitsOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"agent not found: \"agent-7\""}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"agent-7", "--lane", "auger", "--workdir", "/w",
		"--url", srv.URL, "--scheduler-id", "sched-cli",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (not accepted)", code)
	}
	if !strings.Contains(stderr.String(), "NOT dispatched") {
		t.Errorf("stderr = %q, want an explicit NOT-dispatched line", stderr.String())
	}
}

// TestRunDirectiveReachesTheWireAsTextB64 is the assertion that makes the
// directive flags real: it decodes the member the relay will actually receive
// and requires the operator's text back byte-for-byte. A test that only
// checked the flag parsed would pass on a surface that drops the value.
func TestRunDirectiveReachesTheWireAsTextB64(t *testing.T) {
	var stdout, stderr bytes.Buffer
	const directive = "read the board, pick the top open row, work it"
	code := run([]string{
		"agent-7", "--lane", "code-factory", "--board", "/b/tasks.jsonl",
		"--corr-id", "q-cli-text", "--scheduler-id", "sched-cli",
		"--text", directive, "--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr=%s", code, stderr.String())
	}
	body := strings.TrimSpace(stdout.String()[strings.Index(stdout.String(), "body: ")+len("body: "):])
	var req struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("dry-run body is not JSON: %v (%s)", err, body)
	}
	enc, ok := req.Payload["text_b64"].(string)
	if !ok {
		t.Fatalf("payload carries no text_b64 member: %v", req.Payload)
	}
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("text_b64 is not base64: %v", err)
	}
	if string(dec) != directive {
		t.Errorf("decoded directive = %q, want %q", string(dec), directive)
	}
	// The text must NOT also ride in the clear: the envelope is the thing the
	// relay logs, and a directive that leaks there defeats the base64 carrier.
	if _, leaked := req.Payload["text"]; leaked {
		t.Errorf("payload carries a plaintext text member alongside text_b64: %v", req.Payload)
	}
}

// TestRunRefusesTwoDirectivesAndAnUnreadableFile pins the refusals that keep
// this surface honest: a caller with two directives has not decided which one
// the node runs, and both cases must exit 2 having attempted nothing.
func TestRunRefusesTwoDirectivesAndAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "brief.txt")
	if err := os.WriteFile(good, []byte("do the thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both", []string{"--text", "a", "--text-file", good}, "mutually exclusive"},
		{"missing", []string{"--text-file", filepath.Join(dir, "nope.txt")}, "no such file"},
		{"empty", []string{"--text-file", empty}, "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"agent-7", "--lane", "code-factory", "--board", "/b/tasks.jsonl", "--dry-run"}, tc.args...)
			if code := run(args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2 (usage)\nstderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("nothing should be rendered, got %q", stdout.String())
			}
		})
	}
}

// TestRunTextFileIsTheDocumentedCarrierForALongPrompt proves the file form is
// the same member as --text (one carrier, two entry points, no drift).
func TestRunTextFileIsTheDocumentedCarrierForALongPrompt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "brief.md")
	const directive = "line one\nline 'two' with \"quotes\" and $dollars\n"
	if err := os.WriteFile(p, []byte(directive), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"agent-7", "--lane", "code-factory", "--board", "/b/tasks.jsonl",
		"--corr-id", "q-cli-file", "--scheduler-id", "sched-cli",
		"--text-file", p, "--dry-run",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr=%s", code, stderr.String())
	}
	body := strings.TrimSpace(stdout.String()[strings.Index(stdout.String(), "body: ")+len("body: "):])
	var req struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("dry-run body is not JSON: %v", err)
	}
	dec, err := base64.StdEncoding.DecodeString(req.Payload["text_b64"].(string))
	if err != nil {
		t.Fatalf("text_b64 is not base64: %v", err)
	}
	if string(dec) != directive {
		t.Errorf("decoded directive = %q, want the file's bytes %q", string(dec), directive)
	}
}
