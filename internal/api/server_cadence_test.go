package api

// SOL-CADENCE / SCHED-GAP-1668: GET /api/v1/cadence — the configured-vs-achieved
// cadence surface. The endpoint reads two sources: projects.target_runs_per_day
// (with the durable cooldown pin as the derived fallback) and the trailing
// count of spawned ticks. This file pins the whole contract a consumer depends
// on — the four target sources, the achieved rate, the window, the deficit
// arithmetic and the GET-only method gate — at the handler, with the DB seeded
// directly.
//
// Clock seam (SCHED-GAP-169): the Server reads time through clock.NewFixed, so
// the window and the seeded spawn stamps are exact instants, never wall-clock
// relative. A fresh Server built with a nil loop has an EMPTY clock seam, so
// installing the fixed clock here is the first store (a Seam can only ever hold
// one concrete Clock type).

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// cadenceNow is the fixed decision instant for this file; every stamp below is
// derived from it.
var cadenceNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// cadenceRequest drives the wired handler and returns status + decoded body.
func cadenceRequest(t *testing.T, s *Server, method string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/cadence", nil))
	var body map[string]interface{}
	if raw := rec.Body.Bytes(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode /api/v1/cadence (%d): %v (body: %s)", rec.Code, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestAPI_Cadence_ConfiguredVsAchieved(t *testing.T) {
	// The endpoint's own window constant is part of the served contract: the
	// JSON's window_days is derived from it.
	if cadenceWindow != 7*24*time.Hour {
		t.Fatalf("cadenceWindow = %v, want 7d", cadenceWindow)
	}

	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewServer(db, nil)
	s.SetClock(clock.NewFixed(cadenceNow))

	ctx := clock.WithClock(context.Background(), clock.NewFixed(cadenceNow))

	pin := 21600 // 6h durable pin → derived target 86400/21600 = 4 runs/day
	override := 3.0
	optOut := 0.0
	lanes := []*database.Project{
		{
			Name: "a-pinned", RepoURL: "local://a-pinned", Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: pin,
			DecayRate: 1, Enabled: true,
		},
		{
			Name: "b-override", RepoURL: "local://b-override", Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: 21600, TargetRunsPerDay: &override,
			DecayRate: 1, Enabled: true,
		},
		{
			// An explicit zero is a DECLARED opt-out, distinct from silence.
			Name: "c-opt-out", RepoURL: "local://c-opt-out", Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: 21600, TargetRunsPerDay: &optOut,
			DecayRate: 1, Enabled: true,
		},
		{
			// Neither override nor pin → no cadence opinion at all.
			Name: "d-none", RepoURL: "local://d-none", Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: 21600,
			DecayRate: 1, Enabled: true,
		},
	}
	for _, p := range lanes {
		if err := database.CreateProject(ctx, db, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.Name, err)
		}
	}
	// The durable pin is an UPDATE-only attribute (CreateProject deliberately
	// does not carry cooldown_pin_s — the pin is operator state applied through
	// UpdateProject / the fleet.toml pin pass), so set it the way the daemon
	// does and prove the derived path reads it back.
	for _, name := range []string{"a-pinned", "c-opt-out"} {
		if err := database.UpdateProject(ctx, db, name, database.ProjectUpdates{CooldownPinS: &pin}); err != nil {
			t.Fatalf("UpdateProject(pin %s): %v", name, err)
		}
		got, err := database.GetProject(ctx, db, name)
		if err != nil {
			t.Fatalf("GetProject(%s): %v", name, err)
		}
		if got.CooldownPinS == nil || *got.CooldownPinS != pin {
			t.Fatalf("%s cooldown_pin_s = %#v, want the %ds pin persisted", name, got.CooldownPinS, pin)
		}
	}
	// Two spawned runs for a-pinned inside the trailing window → 2/7 per day.
	for i, id := range []string{"cad-spawn-1", "cad-spawn-2"} {
		tick := &database.Tick{ID: id, ProjectName: "a-pinned", Status: database.StatusQueued}
		if err := database.CreateTick(ctx, db, tick); err != nil {
			t.Fatalf("CreateTick(%s): %v", id, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE ticks SET spawned_at = ?, status = 'running' WHERE id = ?`,
			cadenceNow.Add(-time.Duration(i+1)*time.Hour).Format(time.RFC3339), id); err != nil {
			t.Fatalf("stamp spawned_at(%s): %v", id, err)
		}
	}

	code, body := cadenceRequest(t, s, http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/cadence = %d, want 200 (body: %v)", code, body)
	}
	raw, ok := body["cadence"].([]interface{})
	if !ok {
		t.Fatalf("cadence field missing or not a list: %v", body)
	}
	items := make(map[string]map[string]interface{}, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]interface{})
		if !ok {
			t.Fatalf("cadence item is not an object: %v", it)
		}
		name, _ := m["name"].(string)
		items[name] = m
	}
	for _, want := range []string{"a-pinned", "b-override", "c-opt-out", "d-none"} {
		if _, ok := items[want]; !ok {
			t.Fatalf("lane %q missing from /api/v1/cadence (%v)", want, items)
		}
	}

	num := func(m map[string]interface{}, key string) float64 {
		t.Helper()
		v, ok := m[key].(float64)
		if !ok {
			t.Fatalf("%s = %#v, want a number", key, m[key])
		}
		return v
	}

	// a-pinned: derived target, two achieved runs, deficit = 4 - 2/7.
	p := items["a-pinned"]
	if got := p["target_source"]; got != "cooldown_pin" {
		t.Errorf("a-pinned target_source = %v, want cooldown_pin", got)
	}
	if got := num(p, "target_runs_per_day"); math.Abs(got-4) > 1e-9 {
		t.Errorf("a-pinned target_runs_per_day = %v, want 4", got)
	}
	if got := num(p, "achieved_runs_per_day"); math.Abs(got-2.0/7.0) > 1e-9 {
		t.Errorf("a-pinned achieved_runs_per_day = %v, want 2/7", got)
	}
	if got := num(p, "target_deficit_per_day"); math.Abs(got-(4-2.0/7.0)) > 1e-9 {
		t.Errorf("a-pinned target_deficit_per_day = %v, want 4-2/7", got)
	}
	if got := num(p, "window_days"); got != 7 {
		t.Errorf("a-pinned window_days = %v, want 7", got)
	}

	// b-override: the explicit target wins, nothing achieved yet.
	if o := items["b-override"]; o["target_source"] != "override" ||
		math.Abs(num(o, "target_runs_per_day")-3) > 1e-9 ||
		num(o, "target_deficit_per_day") != 3 {
		t.Errorf("b-override item = %v, want source=override target=3 deficit=3", o)
	}

	// c-opt-out: explicit zero is reported as a declared opt-out with a null
	// target — a NULL that carries its reason, never a fabricated number.
	if o := items["c-opt-out"]; o["target_source"] != "disabled" || o["target_runs_per_day"] != nil {
		t.Errorf("c-opt-out item = %v, want source=disabled and a null target", o)
	}

	// d-none: no override, no pin → source "none" and honest zeros.
	d := items["d-none"]
	if d["target_source"] != "none" || d["target_runs_per_day"] != nil {
		t.Errorf("d-none item = %v, want source=none and a null target", d)
	}
	if num(d, "achieved_runs_per_day") != 0 || num(d, "target_deficit_per_day") != 0 {
		t.Errorf("d-none item = %v, want zero achieved and zero deficit", d)
	}

	// Read-only: every other method is refused.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if c, _ := cadenceRequest(t, s, method); c != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/cadence = %d, want 405", method, c)
		}
	}
}
