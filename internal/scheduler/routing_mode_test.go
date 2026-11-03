package scheduler

import "testing"

// Bane 2026-11-03 (per-lane routing policy): the orchestration policy resolves
// lane > namespace > global, exactly like the model chains, so a project with
// a hard-coded model can be pinned without flipping the daemon.
func TestRoutingModePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		global    string
		project   string
		namespace string
		want      string
	}{
		{"lane fixed beats global router", "router", "fixed", "", "fixed"},
		{"lane router beats global fixed", "fixed", "router", "", "router"},
		{"lane beats namespace", "router", "router", "fixed", "router"},
		{"namespace inherits when lane empty", "router", "", "fixed", "fixed"},
		{"global default when nothing set", "fixed", "", "", "fixed"},
		{"global defaults to router", "", "", "", "router"},
		{"typo on lane inherits namespace, never unpins silently", "router", "fxed", "fixed", "fixed"},
		{"typo on lane with no namespace falls to global", "fixed", "FIXD", "", "fixed"},
		{"whitespace and case normalise", "router", "  FiXeD  ", "", "fixed"},
	}
	for _, tc := range cases {
		s := &Spawner{foremanRoutingMode: tc.global}
		if s.foremanRoutingMode == "" {
			s.foremanRoutingMode = "router"
		}
		p := PackedProject{RoutingMode: tc.project, NamespaceRoutingMode: tc.namespace}
		if got := s.routingMode(p); got != tc.want {
			t.Errorf("%s: routingMode=%q want %q", tc.name, got, tc.want)
		}
	}
}

// A lane pinned to fixed must not consult the router at all, regardless of the
// lane class — that is the "hard-coded model actually gets used" guarantee.
func TestFixedForemanHonoursLanePolicy(t *testing.T) {
	s := &Spawner{foremanRoutingMode: "router"}
	p := PackedProject{Name: "code-factory", LaneClass: LaneClassForeman, RoutingMode: "fixed"}
	if !s.fixedForeman(p) {
		t.Fatal("lane routing_mode=fixed must pin the foreman even when the daemon default is router")
	}
	// And the inverse: a lane that explicitly asks for router keeps routing.
	p.RoutingMode = "router"
	if s.fixedForeman(p) {
		t.Fatal("lane routing_mode=router must not be pinned by the daemon default")
	}
}

// The pin is the fallback when the router cannot serve. mergePinAhead must put
// it first and never leave a duplicate behind (one POST per pair per tick).
func TestMergePinAhead(t *testing.T) {
	pinned := chainEntry{model: "gpt-6.1-sol", provider: "openai-codex"}
	base := []chainEntry{
		{model: "deepseek-v4-pro", provider: "deepseek-foreman"},
		pinned,
		{model: "global-m", provider: "global-p"},
	}
	got := mergePinAhead(base, pinned)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (pin moved, not duplicated): %+v", len(got), got)
	}
	if got[0] != pinned {
		t.Fatalf("head=%+v want the pin first", got[0])
	}
	seen := map[chainEntry]int{}
	for _, e := range got {
		seen[e]++
		if seen[e] > 1 {
			t.Fatalf("duplicate hop %+v in %+v", e, got)
		}
	}
	// The fallback tail survives: a 401/403 retry must still reach it.
	if got[1].model != "deepseek-v4-pro" || got[2].model != "global-m" {
		t.Fatalf("tail order lost: %+v", got)
	}
}

// A half-configured pin is not a pin: no empty-provider hop may be minted.
func TestPinEntryRequiresBothHalves(t *testing.T) {
	ok := func(p PackedProject) bool { return pinEntry(p) != chainEntry{} }
	if ok(PackedProject{Model: "m", Provider: ""}) {
		t.Error("model without provider must not count as a pin")
	}
	if ok(PackedProject{Model: "", Provider: "p"}) {
		t.Error("provider without model must not count as a pin")
	}
	if ok(PackedProject{Model: " ", Provider: " "}) {
		t.Error("whitespace is not a pin")
	}
	if e := pinEntry(PackedProject{Model: " m ", Provider: " p "}); e.model != "m" || e.provider != "p" {
		t.Errorf("trim failed: %+v", e)
	}
}
