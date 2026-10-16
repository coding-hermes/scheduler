package scheduler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHarness001InvalidMode(t *testing.T) {
	for _, mode := range []string{"typo", "FIXED", " fixed "} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", mode)
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(fmt.Sprint(r), "SCHEDULER_FOREMAN_ROUTING_MODE") {
					t.Fatalf("constructor rejection = %v, want named routing-mode error", r)
				}
			}()
			NewSpawner(nil, 1)
		})
	}
}

func harness001Router(t *testing.T, s *Spawner) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "invoked")
	script := filepath.Join(t.TempDir(), "router")
	body := "#!/bin/sh\nprintf invoked > '" + marker + "'\ncat <<'JSON'\n" + routerRouterJSON + "\nJSON\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	s.SetRouterClient(NewRouterClient([]string{script}, 0))
	return marker
}

func harness001Spawner(t *testing.T, gw *chainGatewayServer) *Spawner {
	t.Helper()
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)
	s := NewSpawner(newTestDB(t), 4)
	s.SetGatewayClient(NewGatewayClient(srv.URL, "", 5*time.Second))
	s.SetNoExecFallback(true)
	s.model, s.provider = "global-model", "global-provider"
	s.fallbackModel, s.fallbackProvider = "global-fallback-model", "global-fallback-provider"
	s.idleModel, s.idleProvider = "global-idle-model", "global-idle-provider"
	return s
}

func harness001Project(t *testing.T) PackedProject {
	t.Helper()
	dir := t.TempDir()
	writeGap065Board(t, dir, `{"id":"DONE","status":"complete"}`)
	return PackedProject{Name: "harness001", LaneClass: LaneClassForeman, Workdir: dir,
		Model: "project-model", Provider: "project-provider",
		ModelChain: `["chain-model@chain-provider"]`, NamespaceChain: `["namespace-model@namespace-provider"]`,
		FallbackModel: "fallback-model", FallbackProvider: "fallback-provider", IdleModel: "idle-model", IdleProvider: "idle-provider"}
}

func TestHarness001Routing(t *testing.T) {
	for _, tc := range []struct {
		name, mode, class string
		fixed             bool
	}{
		{"unset", "", LaneClassForeman, false}, {"router", "router", LaneClassForeman, false},
		{"fixed", "fixed", LaneClassForeman, true}, {"satellite", "fixed", LaneClassSatellite, false},
		{"legacy", "fixed", "", false}, {"unknown", "fixed", "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", tc.mode)
			gw := &chainGatewayServer{}
			s := harness001Spawner(t, gw)
			s.SetPendingCounter(NewPendingTaskCounter(0))
			marker := harness001Router(t, s)
			p := harness001Project(t)
			p.LaneClass = tc.class
			// Construction freezes policy: changing env now must not change this tick.
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "router")
			tick, err := s.Spawn(p, "harness001-routing")
			if err != nil {
				t.Fatal(err)
			}
			if tick == nil {
				t.Fatal("nil tick")
			}
			tick.Wait()
			_, markerErr := os.Stat(marker)
			if tc.fixed && !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("fixed router invoked: %v", markerErr)
			}
			if !tc.fixed && markerErr != nil {
				t.Fatalf("router not invoked: %v", markerErr)
			}
			got := gw.snapshot()
			if len(got) != 1 {
				t.Fatalf("dispatches=%v", got)
			}
			wantM, wantP := "router-model", "router-provider"
			if tc.fixed {
				wantM, wantP = p.Model, p.Provider
			}
			if got[0].model != wantM || got[0].provider != wantP {
				t.Fatalf("dispatch=%v want %s/%s", got, wantP, wantM)
			}
		})
	}
}

func TestHarness001MissingPair(t *testing.T) {
	for _, missing := range []string{"model", "provider"} {
		for _, transport := range []string{"gateway", "exec"} {
			t.Run(missing+"/"+transport, func(t *testing.T) {
				t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "fixed")
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) }))
				defer srv.Close()
				s := NewSpawner(newTestDB(t), 1)
				s.SetNoExecFallback(transport == "gateway")
				if transport == "gateway" {
					s.SetGatewayClient(NewGatewayClient(srv.URL, "", time.Second))
				}
				marker := harness001Router(t, s)
				p := harness001Project(t)
				if missing == "model" {
					p.Model = " "
				} else {
					p.Provider = ""
				}
				dir := t.TempDir()
				execMarker := filepath.Join(dir, "exec-invoked")
				if err := os.WriteFile(filepath.Join(dir, "hermes"), []byte("#!/bin/sh\ntouch '"+execMarker+"'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				tick, err := s.Spawn(p, "harness001-missing")
				if err == nil || !strings.Contains(err.Error(), "fixed foreman route") || !strings.Contains(err.Error(), missing) {
					t.Fatalf("tick=%v err=%v", tick, err)
				}
				if tick != nil {
					t.Fatal("invalid pair started a tick")
				}
				if requests.Load() != 0 {
					t.Fatal("gateway contacted for invalid pair")
				}
				for _, path := range []string{marker, execMarker} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("unexpected side effect %s: %v", path, err)
					}
				}
			})
		}
	}
}

func TestHarness001CustomCommand(t *testing.T) {
	for _, mode := range []string{"router", "fixed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", mode)
			gw := &chainGatewayServer{}
			s := harness001Spawner(t, gw)
			s.SetTickPushDisabled(true)
			routerMarker := harness001Router(t, s)
			p := harness001Project(t)
			p.Model, p.Provider = "", ""
			marker := filepath.Join(p.Workdir, "command-invoked")
			p.Command = filepath.Join(p.Workdir, "custom-command")
			if err := os.WriteFile(p.Command, []byte("#!/bin/sh\nprintf 'invoked\\n' >> '"+marker+"'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			tick, err := s.Spawn(p, "harness001-custom")
			if err != nil {
				t.Fatal(err)
			}
			if tick == nil {
				t.Fatal("nil custom-command tick")
			}
			if outcome := tick.Wait(); outcome.ExitCode != 0 {
				t.Fatalf("command outcome=%+v", outcome)
			}
			raw, err := os.ReadFile(marker)
			if err != nil || string(raw) != "invoked\n" {
				t.Fatalf("command marker=%q err=%v; want exactly one invocation", raw, err)
			}
			if _, err := os.Stat(routerMarker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("custom command invoked task-router: %v", err)
			}
			if got := gw.snapshot(); len(got) != 0 {
				t.Fatalf("custom command contacted gateway: %v", got)
			}
		})
	}
}

func TestHarness001ExecPair(t *testing.T) {
	t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "fixed")
	dir := t.TempDir()
	capture := filepath.Join(dir, "argv")
	t.Setenv("HARNESS001_CAPTURE", capture)
	if err := os.WriteFile(filepath.Join(dir, "hermes"), []byte("#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$HARNESS001_CAPTURE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s := NewSpawner(newTestDB(t), 1)
	s.SetNoExecFallback(false)
	marker := harness001Router(t, s)
	p := harness001Project(t)
	tick, err := s.Spawn(p, "harness001-exec")
	if err != nil {
		t.Fatal(err)
	}
	if tick == nil {
		t.Fatal("nil exec tick")
	}
	tick.Wait()
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(raw), "\x00")
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-m" || args[i] == "--provider" {
			values[args[i]] = args[i+1]
		}
	}
	if values["-m"] != p.Model || values["--provider"] != p.Provider {
		t.Fatalf("exec route=%v", values)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("router invoked: %v", err)
	}
}

func TestHarness001LoopRoutingMode(t *testing.T) {
	t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "router")
	loop := NewLoop(newTestDB(t), time.Minute, time.Hour, 10, 100, 1)
	if err := loop.SetForemanRoutingMode("fixed"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"typo", "FIXED", " fixed "} {
		if err := loop.SetForemanRoutingMode(mode); err == nil || !strings.Contains(err.Error(), "SCHEDULER_FOREMAN_ROUTING_MODE") {
			t.Fatalf("invalid mode %q returned %v, want named validation error", mode, err)
		}
	}
	if got := os.Getenv("SCHEDULER_FOREMAN_ROUTING_MODE"); got != "router" {
		t.Fatalf("setter mutated ambient mode to %q", got)
	}
	gw := &chainGatewayServer{}
	srv := httptest.NewServer(gw.handler())
	defer srv.Close()
	loop.SetGatewayClient(NewGatewayClient(srv.URL, "", time.Second))
	loop.SetNoExecFallback(true)
	routerMarker := harness001Router(t, loop.spawner)
	p := harness001Project(t)
	tick, err := loop.spawner.Spawn(p, "harness001-loop-wiring")
	if err != nil {
		t.Fatal(err)
	}
	if tick == nil {
		t.Fatal("nil loop tick")
	}
	tick.Wait()
	got := gw.snapshot()
	if len(got) != 1 || got[0].model != p.Model || got[0].provider != p.Provider {
		t.Fatalf("explicit fixed wiring dispatches=%v", got)
	}
	if _, err := os.Stat(routerMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit fixed wiring invoked task-router: %v", err)
	}
}

func TestHarness001NoAuthFallback(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Setenv("SCHEDULER_FOREMAN_ROUTING_MODE", "fixed")
			gw := &chainGatewayServer{rejectAll: true}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/responses" && status == 403 {
					// Use the existing recorder but translate its auth response to 403.
					rec := httptest.NewRecorder()
					gw.handler().ServeHTTP(rec, r)
					w.WriteHeader(status)
					_, _ = w.Write(rec.Body.Bytes())
					return
				}
				gw.handler().ServeHTTP(w, r)
			}))
			defer srv.Close()
			s := NewSpawner(newTestDB(t), 1)
			s.SetGatewayClient(NewGatewayClient(srv.URL, "", time.Second))
			s.SetNoExecFallback(true)
			marker := harness001Router(t, s)
			p := harness001Project(t)
			_, err := s.Spawn(p, "harness001-auth")
			if !errors.Is(err, ErrGatewayKeyRejected) {
				t.Fatalf("auth error=%v", err)
			}
			got := gw.snapshot()
			if len(got) != 1 || got[0].model != p.Model || got[0].provider != p.Provider {
				t.Fatalf("fixed dispatches=%v", got)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("router invoked: %v", err)
			}
		})
	}
}
