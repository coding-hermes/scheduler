package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// ── TASK-ROUTER-001: spawn-time task-router resolution ─────────────────────
//
// At spawn time the scheduler asks the task router (router_spawn.py) which
// (provider, model) pair to use for the foreman tick. The router is the
// cost/health authority: it orders every eligible model by price and
// filters out providers that are DOWN/SLOW (health-state.json), quota-gated
// (quota-state.json), or circuit-open (circuit-state.json). Its head is the
// cheapest healthy option — a legitimate PAYG (deepseek) hop whenever price
// ranks it there.
//
// FAIL-OPEN CONTRACT (the router is a suggestion, never a gate):
//   - a nil/empty RouterClient (no SCHEDULER_ROUTER_CMD env, no setter) is
//     "not configured" — spawns resolve exactly as before the router;
//   - missing script, non-zero exit, non-JSON output, router error object,
//     timeout, null head, or gate != OPEN all yield a fallback signal;
//   - Resolve NEVER returns an error — the caller can only fail open.
//
// The Spawn() integration point sits between the existing chain resolution
// (SCHED-GAP-064/065) and the SPAWN log line, so the router's head feeds
// the gateway POST body, the exec branch, the SPAWN line, and the tick's
// model/provider cost lookup alike. The 401/403 gateway retry semantics
// (nextChainResolution / chain walking) are untouched — that is
// TASK-ROUTER-002's scope.

// routerTimeout bounds a single router invocation. The real router is fast
// (~0.12s, read-only duckdb); this cap only guards against a wedged
// process stalling a spawn.
const routerTimeout = 5 * time.Second

// RouterHead is the router's preferred (provider, model) pair — the head
// of the eligible chain.
type RouterHead struct {
	Hop       int     `json:"hop"`
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	USD1M     float64 `json:"usd_1m"`
	DataClass string  `json:"data_class"`
}

// RouterResult is the parsed subset of the router's JSON output the
// scheduler consumes. Every field the scheduler does not use (full chain,
// exclusions, gate reasons, resolved_at) is deliberately not decoded.
type RouterResult struct {
	Project string      `json:"project"`
	Profile string      `json:"profile"`
	Head    *RouterHead `json:"head"`
	Gate    string      `json:"gate"`
}

// OpenHead returns the router's usable model/provider pair. A pair is
// usable ONLY when the gate is OPEN and the head carries BOTH a model and
// a provider — a null head (NO-OPEN-HOP/NO-CHAIN) or a partial head is a
// fallback signal, never a dispatch target.
func (r *RouterResult) OpenHead() (model, provider string, ok bool) {
	if r == nil || r.Gate != "OPEN" || r.Head == nil {
		return "", "", false
	}
	if r.Head.Model == "" || r.Head.Provider == "" {
		return "", "", false
	}
	return r.Head.Model, r.Head.Provider, true
}

// RouterClient runs the router command for a project and parses its JSON
// output. It is fail-open by construction: every failure mode reports a
// fallback reason, and Resolve never returns an error.
type RouterClient struct {
	// argv is the full command vector. Empty/nil = router disabled
	// (spawns behave exactly as before the router).
	argv []string
	// timeout bounds the invocation; zero = default routerTimeout.
	timeout time.Duration
}

// NewRouterClient creates a RouterClient from a full command vector. argv
// nil/empty disables the router. timeout zero selects the default bound.
func NewRouterClient(argv []string, timeout time.Duration) *RouterClient {
	return &RouterClient{argv: argv, timeout: timeout}
}

// Enabled reports whether the client is configured to run the router.
func (rc *RouterClient) Enabled() bool {
	return rc != nil && len(rc.argv) > 0 && rc.argv[0] != ""
}

// Resolve runs the router for project and returns:
//   - res: the parsed result (zero value when unavailable),
//   - ok:  true when the router returned a usable result (gate OPEN with a
//     full head — callers still check OpenHead for the pair),
//   - reason: a human-readable fallback/warning reason ("" on success).
//
// Every failure mode is a fallback — the caller must NEVER treat a failed
// Resolve as a spawn blocker.
func (rc *RouterClient) Resolve(ctx context.Context, project string) (res RouterResult, ok bool, reason string) {
	if !rc.Enabled() {
		return RouterResult{}, false, "router not configured"
	}

	to := rc.timeout
	if to <= 0 {
		to = routerTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	argv := make([]string, 0, len(rc.argv)+3)
	argv = append(argv, rc.argv...)
	argv = append(argv, project, "--format", "json")

	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	// WaitDelay (Go 1.20+): the context kill targets only the DIRECT
	// child. If that child spawned a grandchild (e.g. a shell running
	// sleep), the orphan inherits the stdout/stderr pipe write-ends and
	// cmd.Run() would otherwise block until THEY exit — a wedged router
	// could stall the spawn well past the timeout. WaitDelay makes Run()
	// abandon the pipes shortly after the kill, so the invocation is
	// bounded at ~2× the timeout in the worst case.
	cmd.WaitDelay = to
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			return RouterResult{}, false, fmt.Sprintf("router timed out after %s", to)
		}
		return RouterResult{}, false, fmt.Sprintf("router exec error: %v", err)
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return RouterResult{}, false, fmt.Sprintf("router empty output (stderr: %s)", strings.TrimSpace(stderr.String()))
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return RouterResult{}, false, fmt.Sprintf("router non-JSON output: %v", err)
	}
	if res.Gate == "" && res.Head == nil && res.Project == "" {
		return RouterResult{}, false, "router error: " + truncate(string(out), 200)
	}
	if res.Gate != "OPEN" {
		return res, false, fmt.Sprintf("router gate=%s — no open head", res.Gate)
	}
	if _, _, headOK := res.OpenHead(); !headOK {
		return res, false, "router head incomplete (missing model or provider)"
	}
	return res, true, ""
}

// truncate shortens a string to n runes for log lines.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// resolveRouterHead asks the router for the project's cheapest healthy
// model/provider pair and logs a provenance line either way. Returns the
// router's head when the router is enabled AND returns an open head; a nil
// client, a disabled client, or any router failure yields the fallback
// signal and a ROUTER warning. The spawn continues with the chain values
// — the router is a suggestion, never a gate.
func (s *Spawner) resolveRouterHead(project PackedProject) (model, provider string, used bool) {
	if !s.router.Enabled() {
		log.Printf("ROUTER: %s unavailable (router not configured) — using chain fallback", project.Name)
		return "", "", false
	}
	res, ok, reason := s.router.Resolve(context.Background(), project.Name)
	if !ok {
		log.Printf("ROUTER: %s unavailable — using chain fallback (%s)", project.Name, reason)
		return "", "", false
	}
	m, p, headOK := res.OpenHead()
	if !headOK {
		log.Printf("ROUTER: %s unavailable — using chain fallback (no open head)", project.Name)
		return "", "", false
	}
	log.Printf("ROUTER: %s profile=%s gate=%s head=%s/%s", project.Name, res.Profile, res.Gate, p, m)
	return m, p, true
}
