package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1712 — per-lane and per-namespace gateway endpoints.
//
// The daemon used to resolve ONE gateway URL for every project, so lanes
// could be distinguished by key but never addressed by endpoint. These tests
// pin the three-level resolution (lane > namespace > global) at the two
// levels that matter:
//
//   - the PURE resolver (precedence, per field, independent URL/key), and
//   - SPAWN: two lanes with different endpoints must both dispatch to their
//     OWN endpoint, a lane with no endpoint must inherit its namespace and
//     then the global daemon endpoint, and the resolved fact must land on the
//     tick row with no unexplained blanks.

// endpointProbe is a stub Hermes gateway: it answers /health 200 (the
// per-lane key probe) and counts /v1/responses POSTs, capturing each
// Authorization header.
type endpointProbe struct {
	mu    sync.Mutex
	posts int
	auths []string
}

func (p *endpointProbe) snapshot() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts, append([]string(nil), p.auths...)
}

// newEndpointProbe starts a stub gateway labelled by response id.
func newEndpointProbe(t *testing.T, respID string) (*endpointProbe, *httptest.Server) {
	t.Helper()
	p := &endpointProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		p.mu.Lock()
		p.posts++
		p.auths = append(p.auths, r.Header.Get("Authorization"))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     respID,
			"status": "completed",
			"output": []map[string]any{{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": "ok"},
				},
			}},
			"usage": map[string]int{"input_tokens": 800, "output_tokens": 20, "total_tokens": 820},
		})
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

// TestGatewaySourceSpellingsMatchDatabase pins the two spellings of the tier
// vocabulary together. The scheduler records its local constants on the tick
// row (ticks.gateway_source / gateway_key_source) while the database package
// exports the same three strings; nothing else would catch a rename on one
// side, and the drift would be silent (the columns carry no CHECK).
func TestGatewaySourceSpellingsMatchDatabase(t *testing.T) {
	pairs := []struct {
		scheduler, database string
	}{
		{gatewaySourceLane, database.GatewaySourceLane},
		{gatewaySourceNamespace, database.GatewaySourceNamespace},
		{gatewaySourceGlobal, database.GatewaySourceGlobal},
	}
	for _, p := range pairs {
		if p.scheduler != p.database {
			t.Errorf("tier spelling drift: scheduler %q vs database %q", p.scheduler, p.database)
		}
	}
}

// TestResolveGatewayEndpoint_Precedence pins the pure resolution: lane wins
// over namespace wins over global, PER FIELD, and a blank tier never shadows
// the tier below it.
func TestResolveGatewayEndpoint_Precedence(t *testing.T) {
	cases := []struct {
		name       string
		cfg        GatewayEndpointConfig
		wantURL    string
		wantURLSrc string
		wantKey    string
		wantKeySrc string
	}{
		{
			name: "nothing configured anywhere stays honestly empty",
			cfg:  GatewayEndpointConfig{},
		},
		{
			name:       "global only (the pre-1712 fleet)",
			cfg:        GatewayEndpointConfig{GlobalURL: "http://g:8642", GlobalKey: "gk"},
			wantURL:    "http://g:8642",
			wantURLSrc: gatewaySourceGlobal,
			wantKey:    "gk",
			wantKeySrc: gatewaySourceGlobal,
		},
		{
			name:       "namespace inherits when the lane is empty",
			cfg:        GatewayEndpointConfig{NamespaceURL: "http://ns:8642", NamespaceKey: "nk", GlobalURL: "http://g:8642", GlobalKey: "gk"},
			wantURL:    "http://ns:8642",
			wantURLSrc: gatewaySourceNamespace,
			wantKey:    "nk",
			wantKeySrc: gatewaySourceNamespace,
		},
		{
			name:       "lane wins over namespace and global",
			cfg:        GatewayEndpointConfig{LaneURL: "http://lane:8642", NamespaceURL: "http://ns:8642", GlobalURL: "http://g:8642", LaneKey: "lk", NamespaceKey: "nk", GlobalKey: "gk"},
			wantURL:    "http://lane:8642",
			wantURLSrc: gatewaySourceLane,
			wantKey:    "lk",
			wantKeySrc: gatewaySourceLane,
		},
		{
			name: "URL and key resolve INDEPENDENTLY: lane URL, namespace key",
			cfg: GatewayEndpointConfig{
				LaneURL: "http://lane:8642", NamespaceURL: "http://ns:8642", GlobalURL: "http://g:8642",
				NamespaceKey: "nk", GlobalKey: "gk",
			},
			wantURL:    "http://lane:8642",
			wantURLSrc: gatewaySourceLane,
			wantKey:    "nk",
			wantKeySrc: gatewaySourceNamespace,
		},
		{
			name: "a whitespace-only lane never shadows the tier below it",
			cfg: GatewayEndpointConfig{
				LaneURL: "   ", NamespaceURL: "http://ns:8642", GlobalURL: "http://g:8642",
				LaneKey: "  ", GlobalKey: "gk",
			},
			wantURL:    "http://ns:8642",
			wantURLSrc: gatewaySourceNamespace,
			wantKey:    "gk",
			wantKeySrc: gatewaySourceGlobal,
		},
		{
			name: "a per-lane key over an inherited namespace URL",
			cfg: GatewayEndpointConfig{
				LaneKey: "lk", NamespaceURL: "http://ns:8642", GlobalURL: "http://g:8642", GlobalKey: "gk",
			},
			wantURL:    "http://ns:8642",
			wantURLSrc: gatewaySourceNamespace,
			wantKey:    "lk",
			wantKeySrc: gatewaySourceLane,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveGatewayEndpoint(tc.cfg)
			if got.URL != tc.wantURL || got.URLSource != tc.wantURLSrc {
				t.Errorf("URL = (%q, %q), want (%q, %q)", got.URL, got.URLSource, tc.wantURL, tc.wantURLSrc)
			}
			if got.Key != tc.wantKey || got.KeySource != tc.wantKeySrc {
				t.Errorf("Key = (%q, %q), want (%q, %q)", got.Key, got.KeySource, tc.wantKey, tc.wantKeySrc)
			}
			// No unexplained nulls: source is empty iff its value is empty.
			if (got.URL == "") != (got.URLSource == "") {
				t.Errorf("URL %q paired with source %q — a blank value must carry a blank source and vice versa", got.URL, got.URLSource)
			}
			if (got.Key == "") != (got.KeySource == "") {
				t.Errorf("Key source %q is not paired with the key value", got.KeySource)
			}
		})
	}
}

// TestSpawn_RoutesEachLaneToItsOwnResolvedEndpoint is the acceptance test:
// within ONE tick window three lanes dispatch to three different gateway
// endpoints resolved from the three tiers, and each tick row records the
// endpoint it was addressed to plus the tier that supplied URL and key.
func TestSpawn_RoutesEachLaneToItsOwnResolvedEndpoint(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	probeGlobal, srvGlobal := newEndpointProbe(t, "resp_global")
	probeLane, srvLane := newEndpointProbe(t, "resp_lane")
	probeNS, srvNS := newEndpointProbe(t, "resp_ns")

	// The namespace tier: its URL is inherited by member lanes with no
	// endpoint of their own; its key is deliberately left empty so lane-c
	// proves the KEY tier inherits independently down to the global key.
	nsID := "ns1712"
	if err := database.CreateNamespace(ctx, db, &database.Namespace{
		ID: nsID, Weight: 10, Reserved: 1, HardCap: 100, Enabled: true,
		GatewayURL: srvNS.URL,
	}); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}

	mkProject := func(name string, namespace *string, gwURL string) {
		t.Helper()
		if err := database.CreateProject(ctx, db, &database.Project{
			Name: name, RepoURL: "git@example.invalid:" + name + ".git", Workdir: t.TempDir(),
			Weight: 10, Priority: 5, CooldownS: 7200, Enabled: true,
			NamespaceID: namespace, GatewayURL: gwURL,
		}); err != nil {
			t.Fatalf("CreateProject %s: %v", name, err)
		}
	}
	// lane-a: no endpoint anywhere of its own → inherits the daemon global.
	mkProject("lane1712-a", nil, "")
	// lane-b: its OWN endpoint + its OWN key → the lane tier end to end.
	mkProject("lane1712-b", nil, srvLane.URL)
	if _, err := db.ExecContext(ctx, `UPDATE projects SET gateway_key = ? WHERE name = ?`, "fk-lane-b", "lane1712-b"); err != nil {
		t.Fatalf("set lane key: %v", err)
	}
	// lane-c: endpoint inherited from its namespace, key inherited from the
	// global daemon — the mixed case.
	mkProject("lane1712-c", &nsID, "")

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srvGlobal.URL, "daemon-shared-key", 5*time.Second))
	spawner.SetNoExecFallback(true)

	type laneSpec struct {
		project  PackedProject
		tickID   string
		wantURL  string
		wantSrc  string
		wantKeyS string
	}
	specs := []laneSpec{
		{PackedProject{Name: "lane1712-a", Workdir: t.TempDir()}, "lane1712-a-2026-10-03-20-00-01", srvGlobal.URL, gatewaySourceGlobal, gatewaySourceGlobal},
		{PackedProject{Name: "lane1712-b", Workdir: t.TempDir(), GatewayKey: "fk-lane-b"}, "lane1712-b-2026-10-03-20-00-02", srvLane.URL, gatewaySourceLane, gatewaySourceLane},
		{PackedProject{Name: "lane1712-c", Workdir: t.TempDir()}, "lane1712-c-2026-10-03-20-00-03", srvNS.URL, gatewaySourceNamespace, gatewaySourceGlobal},
	}

	// ONE tick window: every lane is dispatched in the same evaluation
	// cycle, concurrently — a per-endpoint dispatch must not serialize or
	// leak into its neighbours.
	var wg sync.WaitGroup
	ticks := make([]*SpawnedTick, len(specs))
	errs := make([]error, len(specs))
	for i, s := range specs {
		if err := database.CreateTick(ctx, db, &database.Tick{ID: s.tickID, ProjectName: s.project.Name}); err != nil {
			t.Fatalf("CreateTick %s: %v", s.tickID, err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ticks[i], errs[i] = spawner.Spawn(specs[i].project, specs[i].tickID)
		}(i)
	}
	wg.Wait()

	for i, s := range specs {
		if errs[i] != nil {
			t.Fatalf("%s: Spawn: %v", s.project.Name, errs[i])
		}
		outcome := ticks[i].Wait()
		if outcome.Status != TickCompleted {
			t.Fatalf("%s: tick status = %s, want completed", s.project.Name, outcome.Status)
		}
		var gotURL, gotSrc, gotKeySrc string
		if err := db.QueryRowContext(ctx,
			`SELECT gateway_url, gateway_source, gateway_key_source FROM ticks WHERE id = ?`,
			s.tickID).Scan(&gotURL, &gotSrc, &gotKeySrc); err != nil {
			t.Fatalf("%s: read tick endpoint stamp: %v", s.project.Name, err)
		}
		if gotURL != s.wantURL || gotSrc != s.wantSrc {
			t.Errorf("%s: tick endpoint = (%q, %q), want (%q, %q)", s.project.Name, gotURL, gotSrc, s.wantURL, s.wantSrc)
		}
		if gotKeySrc != s.wantKeyS {
			t.Errorf("%s: tick gateway_key_source = %q, want %q", s.project.Name, gotKeySrc, s.wantKeyS)
		}
		if gotURL == "" || gotSrc == "" {
			t.Errorf("%s: tick recorded an unexplained blank endpoint (%q, %q)", s.project.Name, gotURL, gotSrc)
		}
	}

	// Every POST landed on exactly the endpoint its lane resolved to, and
	// the credential tier travelled with it.
	if n, auths := probeGlobal.snapshot(); n != 1 || auths[0] != "Bearer daemon-shared-key" {
		t.Errorf("global endpoint got %d POST(s) %v, want exactly the lane-a dispatch with the daemon key", n, auths)
	}
	if n, auths := probeLane.snapshot(); n != 1 || auths[0] != "Bearer fk-lane-b" {
		t.Errorf("lane endpoint got %d POST(s) %v, want exactly the lane-b dispatch with its own key", n, auths)
	}
	if n, auths := probeNS.snapshot(); n != 1 || auths[0] != "Bearer daemon-shared-key" {
		t.Errorf("namespace endpoint got %d POST(s) %v, want exactly the lane-c dispatch with the inherited global key", n, auths)
	}

	// Two non-global endpoints exist as derived clients; the global lane
	// keeps using the daemon client (no cache entry).
	if got := spawner.endpointCount(); got != 2 {
		t.Errorf("endpointCount = %d, want 2 (lane-b + the namespace endpoint; lane-a uses the daemon client)", got)
	}
}

// TestSpawn_InheritedEndpointIsByteIdenticalToGlobal pins the additive law:
// a lane whose row carries NO endpoint resolves to the daemon client itself —
// same object, no derived client, and the tick stamp names the global tier.
func TestSpawn_InheritedEndpointIsByteIdenticalToGlobal(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	probe, srv := newEndpointProbe(t, "resp_plain")
	if err := database.CreateProject(ctx, db, &database.Project{
		Name: "lane1712-plain", RepoURL: "git@example.invalid:plain.git", Workdir: t.TempDir(), Weight: 10, Priority: 5, CooldownS: 7200, Enabled: true,
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	daemonClient := NewGatewayClient(srv.URL, "daemon-shared-key", 5*time.Second)
	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(daemonClient)
	spawner.SetNoExecFallback(true)

	tickID := "lane1712-plain-2026-10-03-20-10-00"
	if err := database.CreateTick(ctx, db, &database.Tick{ID: tickID, ProjectName: "lane1712-plain"}); err != nil {
		t.Fatalf("CreateTick: %v", err)
	}
	tick, err := spawner.Spawn(PackedProject{Name: "lane1712-plain", Workdir: t.TempDir()}, tickID)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if outcome := tick.Wait(); outcome.Status != TickCompleted {
		t.Fatalf("tick status = %s, want completed", outcome.Status)
	}
	if n, _ := probe.snapshot(); n != 1 {
		t.Fatalf("global endpoint got %d POST(s), want 1", n)
	}
	if got := spawner.endpointCount(); got != 0 {
		t.Errorf("endpointCount = %d, want 0 — an inheriting lane must reuse the daemon client, not derive one", got)
	}
	var gotURL, gotSrc string
	if err := db.QueryRowContext(ctx, `SELECT gateway_url, gateway_source FROM ticks WHERE id = ?`, tickID).Scan(&gotURL, &gotSrc); err != nil {
		t.Fatalf("read tick stamp: %v", err)
	}
	if gotURL != srv.URL || gotSrc != gatewaySourceGlobal {
		t.Errorf("tick endpoint = (%q, %q), want (%q, global)", gotURL, gotSrc, srv.URL)
	}
}
