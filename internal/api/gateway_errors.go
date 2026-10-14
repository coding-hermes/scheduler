package api

import (
	"net/http"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// SCHED-GAP-1654 — the gateway-availability surface.
//
// GET /api/v1/gateway-errors answers the question the 2026-09-27 03:00
// incident (SCHED-GAP-1644) could not: HOW MANY ticks were lost to gateway
// unavailability, split by error class and by lane, in the last 24 hours.
//
// The rows it counts are the per-tick events the spawn path emits through
// scheduler.Spawner.noteGatewayAvailabilityError (component
// scheduler.GatewayAvailabilityEventComponent, class in the details'
// error_class field). Those events exist only when a tick was genuinely NOT
// run because the gateway refused it — a 503 (drain/unavailable), a 429
// (rate limited) or a refused dial — so this endpoint's numbers are losses,
// not "failed requests": a tick rescued by exec fallback, an auth rejection,
// a per-turn stall and our own tick deadline each keep their own
// classification and are excluded by construction.
//
// The aggregation itself lives in scheduler.BuildGatewayErrorsReport — the
// SAME builder the gateway_errors MCP tool serves, so the REST wire and the
// agent-facing surface cannot drift apart (CTL-003 pairs this route with
// that tool). This handler is the transport: method gate, clock, JSON.
//
// Response shape (all fields always present; every known class appears even
// at zero, so a reader never has to distinguish "no losses" from "field
// missing"):
//
//	{
//	  "generated_at": "<RFC3339>",
//	  "window_hours": 24,
//	  "cutoff": "<RFC3339>",        // events strictly older than this are excluded
//	  "total": 12,                  // ticks not run due to gateway availability
//	  "by_class": {"unavailable_503": 7, "rate_limited_429": 3, "connection_refused": 2},
//	  "by_project": [{"project": "lore-foreman", "total": 5, "by_class": {...}}, ...]
//	}
//
// by_project is ordered by total desc, then project name asc — a stable order
// so two reads of the same window render identically.
//
// A DB failure is a 500 with the error text (the operator surface must never
// answer 200 with fabricated zeros). An empty window is a legitimate answer:
// total 0 with the three classes at 0 and by_project an empty (non-nil)
// array.
func (s *Server) handleGatewayErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	report, err := scheduler.BuildGatewayErrorsReport(r.Context(), s.db, s.clock().Now())
	if err != nil {
		writeError(w, 500, "gateway-errors: "+err.Error())
		return
	}
	writeJSON(w, 200, report)
}
