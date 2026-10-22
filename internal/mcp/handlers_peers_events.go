package mcp

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// peersEventsToolTimeout bounds the peer event slice read (SCHED-GAP-1580
// CTL-003 coverage: mirrors GET /api/v1/peers/events + /api/v1/peers/{id}/events).
const peersEventsToolTimeout = 5 * time.Second

// toolPeersEvents mirrors the REMOTE-006 durable per-peer event slice —
// GET /api/v1/peers/events (peerID "") and GET /api/v1/peers/{id}/events
// (peerID set): the last-seen event, the replay high-water mark
// (last_event_id), and the counted dedupe drops. A hollow answer is never
// fabricated: an unknown id is a not-found error and a scheduler that has
// never sent events reads last_event: null / last_event_id: "" — the same
// rendering the REST route answers with (peerEvents in internal/api).
func (s *Server) toolPeersEvents(ctx context.Context, args map[string]interface{}) (string, error) {
	peerID := strings.TrimSpace(getStringArg(args, "id"))
	if peerID != "" {
		if _, err := database.GetPeer(ctx, s.db, peerID); err != nil {
			if errors.Is(err, database.ErrPeerNotFound) {
				return "", errors.New("unknown peer " + peerID + " — register it first (peers_register)")
			}
			return "", err
		}
	}

	cctx, cancel := context.WithTimeout(ctx, peersEventsToolTimeout)
	defer cancel()

	var last *database.RemoteEvent
	if ev, err := database.RemoteLastEvent(cctx, s.db, peerID); err == nil {
		last = ev
	} else if peerID == "" && !errors.Is(err, sql.ErrNoRows) && err.Error() != "" {
		return "", err
	} else if peerID != "" && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	mark, err := database.RemoteLastEventID(cctx, s.db, peerID)
	if err != nil {
		return "", err
	}
	drops, err := database.CountRemoteEventDedupeDrops(cctx, s.db, peerID)
	if err != nil {
		return "", err
	}
	out := map[string]interface{}{
		"scheduler_id":  peerID,
		"last_event":    last,
		"last_event_id": mark,
		"dedupe_drops":  drops,
	}
	if peerID == "" {
		out["scheduler_id"] = "*"
	}
	if last == nil {
		out["last_event"] = nil
	}
	return jsonString(out), nil
}

// cadenceToolTimeout bounds the cadence read (SCHED-GAP-1580 CTL-003
// coverage: mirrors GET /api/v1/cadence).
const cadenceToolTimeout = 5 * time.Second

// toolCadence mirrors GET /api/v1/cadence (SOL-CADENCE): configured-vs-achieved
// run rates per lane over a 7-day window — target (derived-first via
// EffectiveCadenceTarget, null when none is configured), achieved, and the
// never-negative deficit. Same database functions the REST handler runs
// (api.Server.cadence), so the tool cannot disagree with the route.
// SCHED-GAP-1745: every row also names its admission governor
// (admission_governor, "tasks_board" | "cooldown_pin"), and a tasks-admission
// lane never carries a pin-derived target/deficit — the REST route resolves
// admission_mode through database.AdmissionModePaced and this tool runs the
// same functions, so the two surfaces cannot disagree.
func (s *Server) toolCadence(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, cadenceToolTimeout)
	defer cancel()

	projects, err := database.ListProjects(cctx, s.db, false)
	if err != nil {
		return "", err
	}
	namespaces, err := database.ListNamespaces(cctx, s.db, false)
	if err != nil {
		return "", err
	}
	nsModes := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		nsModes[ns.ID] = ns.AdmissionMode
	}
	rates, err := database.LoadCadenceRates(cctx, s.db, clock.Real().Now(), 7*24*time.Hour)
	if err != nil {
		return "", err
	}
	items := make([]map[string]interface{}, 0, len(projects))
	for i := range projects {
		p := projects[i]
		target, source, configured := database.EffectiveCadenceTarget(p)
		governor := scheduler.CadenceGovernorFor(p, nsModes)
		item := map[string]interface{}{
			"name":                  p.Name,
			"target_source":         source,
			"admission_governor":    governor,
			"achieved_runs_per_day": rates[p.Name],
			"window_days":           7,
		}
		if governor == database.CadenceGovernorCooldown && configured {
			deficit := target - rates[p.Name]
			if deficit < 0 {
				deficit = 0
			}
			item["target_runs_per_day"] = target
			item["target_deficit_per_day"] = deficit
		} else if governor == database.CadenceGovernorTasksBoard && source == "cooldown_pin" {
			item["target_source"] = database.CadenceGovernorTasksBoard
		}
		items = append(items, item)
	}
	return jsonString(map[string]interface{}{"cadence": items}), nil
}
