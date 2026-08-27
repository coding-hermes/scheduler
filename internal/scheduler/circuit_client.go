package scheduler

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ── TASK-ROUTER-002: circuit-breaker recording (router_circuit.py) ─────────
//
// On spawn/tick failures the scheduler records the (provider, model) pair
// that ACTUALLY ran into the shared circuit-breaker state via
// router_circuit.py record-failure; on success it records record-success.
// router_spawn.py (the resolve-time router) already EXCLUDES circuit-open
// and health-DOWN/SLOW pairs from the chain head, so the breaker's cooldown
// (5m → double per consecutive failure → 1h cap) becomes the cross-tick
// backoff for a broken pair: the pair is not offered again until
// open_until passes, and the scheduler's in-tick retry advances to the next
// chain hop instead of re-sending the same pair.
//
// FAIL-OPEN CONTRACT (mirrors the RouterClient — the circuit script is a
// side effect, never a gate):
//   - a nil/empty CircuitClient (no SCHEDULER_CIRCUIT_CMD env, no setter)
//     is "not configured" — spawns resolve and retry exactly as before;
//   - missing script, non-zero exit, timeout, or any exec error are logged
//     as a WARN and ignored — a broken circuit script must never fail or
//     stall a spawn;
//   - Record* NEVER returns an error to the spawn path (the boolean return
//     exists only so callers can log provenance); the daemon is never held
//     hostage by the circuit script.

// circuitTimeout bounds a single circuit-script invocation. The real script
// is a tiny atomic json read/write (~10ms); this cap only guards against a
// wedged process stalling a spawn.
const circuitTimeout = 5 * time.Second

// CircuitClient records (provider, model) outcomes into the shared
// circuit-breaker state file via router_circuit.py. It is fire-and-forget
// by construction: RecordFailure/RecordSuccess log a WARN and return false
// on every failure mode, and never return an error.
type CircuitClient struct {
	// argv is the full command vector. Empty/nil = circuit recording
	// disabled (spawns behave exactly as before the breaker).
	argv []string
	// timeout bounds the invocation; zero = default circuitTimeout.
	timeout time.Duration
}

// NewCircuitClient creates a CircuitClient from a full command vector.
// argv nil/empty disables circuit recording. timeout zero selects the
// default bound.
func NewCircuitClient(argv []string, timeout time.Duration) *CircuitClient {
	return &CircuitClient{argv: argv, timeout: timeout}
}

// Enabled reports whether the client is configured to run the circuit
// script.
func (cc *CircuitClient) Enabled() bool {
	return cc != nil && len(cc.argv) > 0 && cc.argv[0] != ""
}

// run invokes the circuit script with the given subcommand arguments and
// reports whether it exited cleanly. Every failure mode returns false —
// the caller logs a WARN and moves on.
func (cc *CircuitClient) run(ctx context.Context, args ...string) bool {
	if !cc.Enabled() {
		return false
	}
	to := cc.timeout
	if to <= 0 {
		to = circuitTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	argv := make([]string, 0, len(cc.argv)+len(args))
	argv = append(argv, cc.argv...)
	argv = append(argv, args...)

	cmd := exec.CommandContext(rctx, argv[0], argv[1:]...)
	// WaitDelay (Go 1.20+): bounds the invocation even when a shell
	// grandchild inherits the pipe write-ends (same rationale as the
	// RouterClient — a wedged child must not stall the spawn).
	cmd.WaitDelay = to
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if rctx.Err() == context.DeadlineExceeded {
			log.Printf("WARN: circuit script timed out after %s: %s", to, strings.Join(argv, " "))
		} else {
			log.Printf("WARN: circuit script error (%v): %s — %s", err, strings.Join(argv, " "), strings.TrimSpace(stderr.String()))
		}
		return false
	}
	return true
}

// RecordFailure opens (or extends) the circuit for the given pair via
// router_circuit.py record-failure. reason is optional (passed through when
// non-empty). Fire-and-forget: returns false on any failure so callers can
// log provenance — never an error.
func (cc *CircuitClient) RecordFailure(ctx context.Context, provider, model, reason string) bool {
	args := []string{"record-failure", provider, model}
	if reason != "" {
		args = append(args, reason)
	}
	if !cc.Enabled() {
		log.Printf("WARN: circuit client not configured — skipping record-failure for %s/%s", provider, model)
		return false
	}
	ok := cc.run(ctx, args...)
	if !ok {
		log.Printf("WARN: record-failure %s/%s not recorded (circuit script unavailable)", provider, model)
	}
	return ok
}

// RecordSuccess closes the circuit for the given pair via router_circuit.py
// record-success. Fire-and-forget: returns false on any failure so callers
// can log provenance — never an error.
func (cc *CircuitClient) RecordSuccess(ctx context.Context, provider, model string) bool {
	if !cc.Enabled() {
		return false
	}
	ok := cc.run(ctx, "record-success", provider, model)
	if !ok {
		log.Printf("WARN: record-success %s/%s not recorded (circuit script unavailable)", provider, model)
	}
	return ok
}

// circuitFromEnv wires the circuit recorder from SCHEDULER_CIRCUIT_CMD
// (TASK-ROUTER-002). Same contract as routerFromEnv: a full shell command
// line split on spaces (e.g. "/home/kara/.hermes/venvs/board/bin/python3
// /home/kara/.hermes/scripts/router_circuit.py"); the subcommand + pair are
// appended at record time. Unset/empty = circuit recording disabled
// (fail-open default). A command that would not survive the split is
// treated as disabled rather than mis-executed.
func circuitFromEnv() *CircuitClient {
	cmd := strings.TrimSpace(os.Getenv("SCHEDULER_CIRCUIT_CMD"))
	if cmd == "" {
		return nil
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return nil
	}
	return NewCircuitClient(parts, 0)
}
