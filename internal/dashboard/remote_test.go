package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"

	"github.com/coding-hermes/scheduler/internal/database"
)

// REMOTE-006 acceptance 4: the dashboard renders the Remote section with
// peers, their last-seen event (kind/status/timestamp), and the staleness
// indicator (the registry's stale-window semantics — stale ≠ down).

// TestRemoteSectionRendersPeerWithLastEventAndStaleness drives the full
// acceptance: register a peer, heartbeat it fresh, store one event, render
// the page and the htmx fragment, and assert every required element.
func TestRemoteSectionRendersPeerWithLastEventAndStaleness(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Two peers: one with traffic (fresh heartbeat), one registered and
	// never heard from (renders STALE with last_contact "" — never "down").
	if err := database.UpsertPeer(ctx, db, &database.Peer{ID: "peer-alpha", URL: "http://alpha:8080", Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertPeer(ctx, db, &database.Peer{ID: "peer-omega"}); err != nil {
		t.Fatal(err)
	}
	if err := database.PeerHeartbeat(ctx, db, "peer-alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.IngestRemoteEvent(ctx, db, &database.RemoteEvent{
		SchedulerID: "peer-alpha",
		EventID:     "peer-alpha-00000000000000000009",
		Kind:        "tick.terminal",
		Project:     "proj-a",
		TickID:      "t-9",
		Status:      "completed",
		IngestedAt:  now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	gen := NewGenerator(db, nil)
	gen.SetClock(clock.NewFixed(now))

	var page strings.Builder
	if err := gen.GenerateRemote(&page); err != nil {
		t.Fatalf("GenerateRemote: %v", err)
	}
	out := page.String()
	for _, want := range []string{
		"Remote",        // the section itself
		"peer-alpha",    // ≥1 peer listed
		"tick.terminal", // last-seen event KIND
		"completed",     // last-seen event STATUS
		"proj-a",        // the event's project
		`data-utc="` + now.Format("2006-01-02T15:04:05"), // the event timestamp (localtime element)
		"fresh", // freshness verdict for the heartbeated peer
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Remote page missing %q", want)
		}
	}
	// The staleness indicator: the never-heartbeated peer renders STALE —
	// the registry's window semantics — and NEVER a "down" state.
	if !strings.Contains(out, "STALE") {
		t.Errorf("Remote page missing STALE indicator for the never-heartbeated peer")
	}
	for _, forbidden := range []string{"DOWN", ">down<"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("Remote page renders %q — the rendering law forbids a down state", forbidden)
		}
	}
	// The last_event timestamp is a localtime element (the shared TZ JS).
	if !strings.Contains(out, `<time class="local"`) {
		t.Error("Remote page: last-event timestamp must render as a <time class=local> element")
	}

	// The htmx fragment renders the same rows (the /remote/partial body).
	var frag strings.Builder
	if err := gen.GenerateRemoteRows(&frag); err != nil {
		t.Fatalf("GenerateRemoteRows: %v", err)
	}
	fout := frag.String()
	for _, want := range []string{"peer-alpha", "tick.terminal", "completed", "STALE"} {
		if !strings.Contains(fout, want) {
			t.Errorf("Remote fragment missing %q", want)
		}
	}
	// The fragment must NOT carry a full page (it swaps into its own poller).
	if strings.Contains(fout, "<!DOCTYPE html>") {
		t.Error("Remote fragment must be a fragment, not a full page")
	}
}

// TestRemoteSectionEmptyRegistryRendersHonest is the honest-empty arm: no
// peers → the section renders (with the registration hint), not an error.
func TestRemoteSectionEmptyRegistryRendersHonest(t *testing.T) {
	db := newTestDB(t)
	gen := NewGenerator(db, nil)
	gen.SetClock(clock.NewFixed(time.Now().UTC()))
	var buf strings.Builder
	if err := gen.GenerateRemote(&buf); err != nil {
		t.Fatalf("GenerateRemote (empty registry): %v", err)
	}
	if !strings.Contains(buf.String(), "No peers registered") {
		t.Error("empty registry must render the explicit hint, not an empty table")
	}
}

// TestRemoteSectionStalenessFollowsWindow proves the staleness indicator
// reuses the registry's WINDOW semantics: the same peer flips fresh → STALE
// when the window's other edge is driven (window arithmetic, not a constant).
func TestRemoteSectionStalenessFollowsWindow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := database.UpsertPeer(ctx, db, &database.Peer{ID: "peer-window"}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	if err := database.PeerHeartbeat(ctx, db, "peer-window"); err != nil {
		t.Fatal(err)
	}

	gen := NewGenerator(db, nil)
	// A clock far in the future: the heartbeat is now outside ANY sane
	// window → STALE must appear even though last_contact is present.
	gen.SetClock(clock.NewFixed(base.Add(6 * time.Hour)))
	var buf strings.Builder
	if err := gen.GenerateRemote(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "STALE") {
		t.Error("peer heartbeating 6h before render must render STALE (window semantics reused)")
	}
}
