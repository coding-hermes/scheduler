package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// reconnectInterval is the base sleep between reconnector ping cycles.
// In production this is 60s; tests shrink it via the reconnectInterval field
// on the gatewayReconnector.
const reconnectInterval = 60 * time.Second

// reconnectMaxAttempts is the number of backoff retries when a ping fails
// on a previously-connected gateway (the "gateway dropped" path). A
// fallback-start daemon (gwConnected==false) does a single ping per cycle
// — the 60s sleep is the backoff.
const reconnectMaxAttempts = 10

// gatewayPinger is the minimal interface the reconnector needs from a
// gateway client. scheduler.GatewayClient satisfies this naturally.
type gatewayPinger interface {
	Ping(ctx context.Context) error
}

// gatewayReconnector runs a background loop that re-pings the Hermes gateway
// every reconnectInterval. It handles two cases (GAP-048):
//
//  1. gwConnected==true and ping fails → the gateway dropped mid-run. Enter
//     an inner backoff retry loop (up to reconnectMaxAttempts). On success,
//     call onConnect and keep gwConnected=true. If all retries fail,
//     set gwConnected=false so subsequent cycles fall through to case 2.
//
//  2. gwConnected==false (fallback-start or post-drop) → ping the gateway.
//     On success, call onConnect and set gwConnected=true. This is the fix
//     for the original bug: a daemon that STARTED in fallback mode never
//     attempted reconnection, so even a healthy gateway left the fleet
//     exec-spawning forever.
//
// The onConnect callback is invoked exactly once per successful (re)connection
// — it typically calls loop.SetGatewayClient(gwClient) to re-engage HTTP.
type gatewayReconnector struct {
	client    gatewayPinger
	onConnect func()
	connected *atomic.Bool  // shared with the caller (startup path)
	interval  time.Duration // production: 60s; tests shrink
	url       string        // for log messages
	// clk is the wait seam (SCHED-GAP-169): the reconnect cadence and the
	// drop-backoff waits run on it, so a simulated daemon does not sleep in
	// real time. Nil keeps the wall clock.
	clk clock.Clock
}

// runGatewayReconnector launches the background reconnector goroutine and
// returns immediately. The goroutine runs until ctx is cancelled.
//
// gwConnected is an atomic.Bool shared between the startup wiring and the
// reconnector goroutine: the startup path sets it to true on initial connect,
// and the reconnector updates it on subsequent (re)connects or drops.
func runGatewayReconnector(ctx context.Context, client gatewayPinger, onConnect func(), gwConnected *atomic.Bool, gatewayURL string, clk clock.Clock) {
	rc := &gatewayReconnector{
		client:    client,
		onConnect: onConnect,
		clk:       clk,
		connected: gwConnected,
		interval:  reconnectInterval,
		url:       gatewayURL,
	}
	go rc.run(ctx)
}

func (rc *gatewayReconnector) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-rc.clock().After(rc.interval):
		}

		if rc.connected.Load() {
			// Gateway was connected — ping to check it's still up.
			if err := rc.client.Ping(ctx); err != nil {
				log.Printf("WARN: gateway %s dropped (%v) — retrying...", rc.url, err)
				rc.reconnectWithBackoff(ctx)
			}
			// If ping succeeded, nothing to do — still connected.
		} else {
			// Gateway was NOT connected (fallback-start or post-drop).
			// This is the GAP-048 fix: the original code had
			// `if !gwConnected { continue }` here, so a fallback-start
			// daemon never re-pinged. Now we ping every cycle.
			if err := rc.client.Ping(ctx); err == nil {
				rc.connected.Store(true)
				if rc.onConnect != nil {
					rc.onConnect()
				}
				log.Printf("GATEWAY: connected to %s — HTTP API re-engaged", rc.url)
			}
		}
	}
}

// reconnectWithBackoff handles the "gateway dropped" path: up to
// reconnectMaxAttempts pings with increasing backoff. On success, calls
// onConnect and sets gwConnected=true. On total failure, sets
// gwConnected=false so the outer loop falls into the fallback-start path
// (re-ping every interval).
func (rc *gatewayReconnector) reconnectWithBackoff(ctx context.Context) {
	for attempt := 0; attempt < reconnectMaxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wait := time.Duration(attempt+1) * 2 * time.Second
		select {
		case <-ctx.Done():
			return
		case <-rc.clock().After(wait):
		}
		if err := rc.client.Ping(ctx); err == nil {
			rc.connected.Store(true)
			if rc.onConnect != nil {
				rc.onConnect()
			}
			log.Printf("GATEWAY: reconnected to %s", rc.url)
			return
		}
	}
	// All retries failed — mark as disconnected so the outer loop
	// switches to the fallback-start re-ping path.
	rc.connected.Store(false)
	log.Printf("WARN: gateway %s unreachable after %d reconnect attempts — will retry in %v", rc.url, reconnectMaxAttempts, rc.interval)
}

// clock returns the reconnector's clock, never nil.
func (rc *gatewayReconnector) clock() clock.Clock {
	if rc.clk != nil {
		return rc.clk
	}
	return clock.Real()
}
