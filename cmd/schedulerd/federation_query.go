package main

// REMOTE-009 (docs/federation-query-spec.md §3, the "Crier bus" row): the
// bus transport for the federation QUERY surface — daemon wiring.
//
// The ANSWER side lives here: the daemon's query responder subscribes to
// `fed.query.<self>` and hands every §2.1 envelope to the SAME internal
// query entry point the HTTP surface uses (api.Server.FederationBusHandler,
// REMOTE-008's federationOps dispatch + shared replay window), then
// publishes the §2.2 Response envelope on the requester's correlated reply
// topic. A bus answer is therefore computed by exactly the code an HTTP
// answer is computed by (spec §3: "An adapter that computes an answer
// itself is a bug").
//
// THE AUTONOMY LAW: a disabled bus yields a nil responder (a documented
// no-op — nothing ever dials); every runtime failure inside the responder's
// Run loop is log-and-retry; neither boot nor a tick can ever block on it.
// The ASK side is the library entry (bus.Client.Query) — consumed by the
// aggregate (REMOTE-012) and the CLI (REMOTE-011), not by this daemon.

import (
	"context"
	"log"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/bus"
)

// federationQueryResponder builds the daemon's bus query responder for the
// api server. nil when the bus is disabled or the api server is absent —
// the documented off posture (the daemon then answers no bus queries).
func federationQueryResponder(c *bus.Client, apiServer *api.Server) *bus.QueryResponder {
	if c == nil || !c.Enabled() || apiServer == nil {
		return nil
	}
	return bus.NewQueryResponder(c, apiServer.FederationBusHandler)
}

// startFederationQueryResponder launches the responder in the background.
// A nil/disabled responder is a logged no-op (never an error, never a
// blocked boot); a running one is torn down via Close at shutdown.
func startFederationQueryResponder(responder *bus.QueryResponder) {
	if responder == nil || !responder.Enabled() {
		log.Printf("CRIER: federation query responder disabled (no-op)")
		return
	}
	log.Printf("CRIER: federation query responder answering on %s", responder.Topic())
	go responder.Run(context.Background())
}
