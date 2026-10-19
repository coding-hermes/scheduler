package bus

// SCHED-GAP-1665 — DispatchTo: the same hand-out over an EXPLICIT relay base
// URL, which is how a targeted tick reaches a registered agent whose endpoint
// is a different relay than the scheduler's own.

import (
	"context"
	"net/http/httptest"
	"testing"
)

// TestDispatchToUsesExplicitBaseURL — the endpoint the caller passes wins, the
// wire path is the documented one, and an EMPTY base URL falls back to the
// client's configured relay (so a caller with no per-agent endpoint never needs
// this method).
func TestDispatchToUsesExplicitBaseURL(t *testing.T) {
	explicitRec := &dispatchRecorder{}
	explicit := httptest.NewServer(explicitRec)
	defer explicit.Close()
	configuredRec := &dispatchRecorder{}
	configured := httptest.NewServer(configuredRec)
	defer configured.Close()

	c := newDispatchClient(t, configured.URL)
	item := WorkItem{Lane: "helix", Workdir: "/tmp/helix", CorrID: "q-explicit-1"}

	if _, err := c.DispatchTo(context.Background(), explicit.URL, "helix-agent", item); err != nil {
		t.Fatalf("DispatchTo(explicit): %v", err)
	}
	if got := explicitRec.count(); got != 1 {
		t.Fatalf("explicit relay saw %d hand-outs, want 1", got)
	}
	if got := configuredRec.count(); got != 0 {
		t.Errorf("configured relay saw %d hand-outs on the explicit arm, want 0", got)
	}
	if path := explicitRec.first(t).path; path != "/agents/helix-agent/inbox" {
		t.Errorf("path = %q, want the documented inbox route", path)
	}

	if _, err := c.DispatchTo(context.Background(), "", "helix-agent", item); err != nil {
		t.Fatalf("DispatchTo(empty): %v", err)
	}
	if got := configuredRec.count(); got != 1 {
		t.Errorf("configured relay saw %d hand-outs for an empty base URL, want 1", got)
	}
	if got := explicitRec.count(); got != 1 {
		t.Errorf("explicit relay saw %d hand-outs after the fallback arm, want still 1", got)
	}
}

// TestDispatchToDisabledClientIsAnError — the loud-failure law is unchanged for
// the explicit-URL path: a disabled client is an error, never a silent no-op,
// and no connection is opened.
func TestDispatchToDisabledClientIsAnError(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	c := NewClient(false, srv.URL, "", "sched-a")
	_, err := c.DispatchTo(context.Background(), srv.URL, "helix-agent", WorkItem{Lane: "helix", Workdir: "/tmp/helix"})
	if err == nil {
		t.Fatal("DispatchTo on a disabled client returned nil, want ErrDisabled")
	}
	if rec.count() != 0 {
		t.Errorf("disabled dispatch opened %d connections, want 0", rec.count())
	}
}
