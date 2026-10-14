package bus

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubRelay is a minimal Crier relay stand-in for the publish lane: it
// records every POST /relay/publish (method, path, headers, body) and
// answers a configurable status.
type stubRelay struct {
	t *testing.T

	mu      sync.Mutex
	methods []string
	paths   []string
	auth    []string
	agent   []string
	bodies  []publishRequest

	status int
}

func newStubRelay(t *testing.T, status int) *stubRelay {
	s := &stubRelay{t: t, status: status}
	return s
}

func (s *stubRelay) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req publishRequest
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.methods = append(s.methods, r.Method)
		s.paths = append(s.paths, r.URL.Path)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.agent = append(s.agent, r.Header.Get("X-Agent-ID"))
		s.bodies = append(s.bodies, req)
		s.mu.Unlock()
		w.WriteHeader(s.status)
	})
}

func (s *stubRelay) URL() string {
	srv := httptest.NewServer(s.handler())
	s.t.Cleanup(srv.Close)
	return srv.URL
}

func (s *stubRelay) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *stubRelay) last() publishRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return publishRequest{}
	}
	return s.bodies[len(s.bodies)-1]
}

func TestPublish_SendsDocumentedEnvelopeOn202(t *testing.T) {
	relay := newStubRelay(t, http.StatusAccepted)
	c := NewClient(true, relay.URL(), "", "alpha", WithClock(func() time.Time {
		return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	}))

	if err := c.PublishTickTerminal(context.Background(), "proj", "tick-1", "completed"); err != nil {
		t.Fatalf("PublishTickTerminal returned %v — publishes are cannot-error", err)
	}
	if relay.count() != 1 {
		t.Fatalf("relay saw %d publishes, want 1", relay.count())
	}
	got := relay.last()
	if got.Topic != "sched.tick.alpha" {
		t.Errorf("topic = %q, want sched.tick.alpha", got.Topic)
	}
	env := got.Event
	if env.SchedulerID != "alpha" || env.EventID == "" || env.Kind != "tick.terminal" ||
		env.Project != "proj" || env.TickID != "tick-1" || env.Status != "completed" {
		t.Errorf("envelope incomplete: %+v", env)
	}
	if _, err := time.Parse(time.RFC3339Nano, env.TS); err != nil {
		t.Errorf("ts %q not RFC3339Nano: %v", env.TS, err)
	}
	if env.TS != "2026-10-01T12:00:00Z" {
		t.Errorf("ts = %q, want the injected clock's stamp", env.TS)
	}
}

func TestPublish_OptInBearerOnlyWhenTokenSet(t *testing.T) {
	withTok := newStubRelay(t, http.StatusAccepted)
	cTok := NewClient(true, withTok.URL(), "sekrit", "alpha")
	if err := cTok.Publish(context.Background(), Envelope{Kind: KindTickTerminal, Project: "p", TickID: "t", Status: "completed"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if a := withTok.auth[len(withTok.auth)-1]; a != "Bearer sekrit" {
		t.Errorf("Authorization = %q, want Bearer sekrit", a)
	}

	noTok := newStubRelay(t, http.StatusAccepted)
	cNo := NewClient(true, noTok.URL(), "", "alpha")
	if err := cNo.Publish(context.Background(), Envelope{Kind: KindTickTerminal, Project: "p", TickID: "t", Status: "completed"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if a := noTok.auth[len(noTok.auth)-1]; a != "" {
		t.Errorf("without token: Authorization = %q, want empty (opt-in auth)", a)
	}
}

func TestPublish_UnexpectedStatusStillCannotError(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500} {
		relay := newStubRelay(t, status)
		c := NewClient(true, relay.URL(), "", "alpha")
		if err := c.PublishTickTerminal(context.Background(), "p", "t", "failed"); err != nil {
			t.Fatalf("status %d: Publish returned %v — non-202 must log-and-drop, never error", status, err)
		}
		if relay.count() != 1 {
			t.Errorf("status %d: relay saw %d publishes, want 1 (the attempt was made)", status, relay.count())
		}
	}
}

func TestPublish_NoOpWhenDisabled(t *testing.T) {
	relay := newStubRelay(t, http.StatusAccepted)
	url := relay.URL()
	c := NewClient(false, url, "", "alpha") // enabled=false

	if c.Enabled() {
		t.Fatal("Enabled() = true on a disabled client")
	}
	// Enough publishes that any real dial would be visible.
	for i := 0; i < 5; i++ {
		if err := c.PublishTickTerminal(context.Background(), "p", "t", "completed"); err != nil {
			t.Fatalf("disabled publish returned %v", err)
		}
	}
	if relay.count() != 0 {
		t.Fatalf("disabled client hit the relay %d times — must be a pure no-op", relay.count())
	}
	// The disabled client is also the nil-safe shape.
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("nil client reports Enabled")
	}
}

func TestEventID_MonotonicPerScheduler(t *testing.T) {
	c := NewClient(true, "http://127.0.0.1:1", "", "alpha")
	prev := ""
	for i := 0; i < 100; i++ {
		id := c.NextEventID()
		if id == prev {
			t.Fatalf("event id %q repeated at %d — must be monotonic", id, i)
		}
		if prev != "" {
			pv, _, ok1 := splitCounter(prev)
			cv, _, ok2 := splitCounter(id)
			if !ok1 || !ok2 || cv != pv+1 {
				t.Fatalf("ids %q → %q are not consecutive counters", prev, id)
			}
		}
		if !strings.HasPrefix(id, "alpha-") {
			t.Fatalf("id %q does not carry the scheduler id prefix", id)
		}
		prev = id
	}
	// Two schedulers never collide: same counter, different prefix.
	c2 := NewClient(true, "http://127.0.0.1:1", "", "beta")
	if c.NextEventID() == c2.NextEventID() {
		t.Fatal("distinct schedulers minted identical event ids")
	}
	// And the parser round-trips.
	owner, ok := ParseEventID(prev)
	if !ok || owner != "alpha" {
		t.Fatalf("ParseEventID(%q) = %q,%v — want alpha,true", prev, owner, ok)
	}
}

func TestPublishFailure_CannotError(t *testing.T) {
	// Nothing listens on this port: every publish dials, fails, logs, drops.
	c := NewClient(true, "http://127.0.0.1:1", "", "alpha")
	for i := 0; i < 3; i++ {
		if err := c.PublishTickTerminal(context.Background(), "p", "t", "completed"); err != nil {
			t.Fatalf("unreachable relay: Publish returned %v — the autonomy law forbids it", err)
		}
	}
}

func TestEnvelope_Validate(t *testing.T) {
	full := Envelope{SchedulerID: "a", EventID: "a-00001", Kind: KindTickTerminal, Project: "p", TickID: "t", Status: "completed"}
	if err := full.Validate(); err != nil {
		t.Errorf("valid envelope rejected: %v", err)
	}
	empty := Envelope{SchedulerID: "a", EventID: "a-00001"}
	if err := empty.Validate(); err == nil {
		t.Error("envelope missing kind/project/tick/status accepted")
	} else if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error should name the missing field: %v", err)
	}
}
