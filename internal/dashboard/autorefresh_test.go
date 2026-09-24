package dashboard_test

import (
	"io"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1606. Operator request: the auto-refresh interval must be changeable
// and must count down so the reader knows when the next update lands.
//
// Before this change the interval was hardcoded PER PAGE and the pages disagreed:
// the overview rendered "htmx live · 10s" and "auto-refresh 60s" on the SAME
// screen, health polled every 10s, the queue and tick history every 30s, and the
// queue/tick polls used hx-swap="none" so they fetched a page and swapped nothing.
// There was no control and no countdown anywhere.
//
// These tests pin the two properties that made that possible: one control on every
// page, and exactly one home for the cadence.
func TestAutoRefresh_ControlOnEveryPageAndNoHardcodedCadence(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	pages := []struct {
		name   string
		render func(io.Writer) error
	}{
		{"overview", gen.Generate},
		{"health", gen.GenerateHealth},
		{"queue", gen.GenerateQueue},
		{"ticks", func(w io.Writer) error { return gen.GenerateTickHistory(w, 1, database.TickFilter{}) }},
	}

	for _, p := range pages {
		var buf strings.Builder
		if err := p.render(&buf); err != nil {
			t.Fatalf("%s: render: %v", p.name, err)
		}
		out := buf.String()

		// The control and the countdown live in the shared layout, so every page
		// must carry them — a page that renders without them is a page whose
		// reported cadence is unverifiable.
		for _, want := range []string{`id="refreshBar"`, `id="rbSel"`, `id="rbCd"`, `value="0">off<`, "ch.autorefresh_ms"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing auto-refresh control element %q", p.name, want)
			}
		}

		// No page may still assert its own cadence. These are the exact strings the
		// old pages carried; a bare "auto-refresh" is allowed (the control's
		// aria-label uses it) but a cadence claim is not.
		for _, bad := range []string{
			"auto-refresh 10s", "auto-refresh 30s", "auto-refresh 60s",
			"auto-refreshing every", "updating in ",
			`every 10s`, `every 30s`, `every 60s`,
		} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: still states a hardcoded refresh cadence: %q", p.name, bad)
			}
		}
	}
}

// The interval must have exactly ONE home: every polling element is driven by the
// single named event the layout driver dispatches, never by its own timer.
func TestAutoRefresh_PollingUsesTheSharedEvent(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	cases := []struct {
		name   string
		render func(io.Writer) error
	}{
		{"overview", gen.Generate},
		{"health", gen.GenerateHealth},
		{"queue", gen.GenerateQueue},
		{"ticks", func(w io.Writer) error { return gen.GenerateTickHistory(w, 1, database.TickFilter{}) }},
	}

	for _, c := range cases {
		var buf strings.Builder
		if err := c.render(&buf); err != nil {
			t.Fatalf("%s: render: %v", c.name, err)
		}
		if !strings.Contains(buf.String(), `hx-trigger="autorefresh from:body"`) {
			t.Errorf("%s: polling is not wired to the shared autorefresh event", c.name)
		}
	}
}
