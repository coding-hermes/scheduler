package bus

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// REMOTE-006 (docs/remote-spec.md §5): the DURABLE half of ingest. The
// subscriber's in-memory seenSet is a receive-path pre-filter only; this
// file adds the store-backed wrapper every caller of Events() drives:
//
//   - IngestEvent persists each inbound envelope keyed by
//     (scheduler_id, event_id) — a duplicate is a COUNTED dedupe drop,
//     never an error (the v58 remote_events PK makes double-apply
//     physically impossible).
//   - ReplaySinceGap recovers a disconnect gap: on (re)subscribe the caller
//     replays everything the store holds for one scheduler above the
//     per-scheduler LAST-EVENT high-water mark. Replay re-ingests through
//     IngestEvent, so it is idempotent by construction — running it twice
//     cannot add a row.
//
// The autonomy law holds: a store failure is a log line and a dropped
// ingest, never an error surfaced to the receive path.

// IngestStore is the durable ingest surface over the remote_events store.
// It wraps *sql.DB narrowly so tests can build it from any migrated
// database handle.
type IngestStore struct {
	db *sql.DB
}

// RemoteIngestResult is the store's ingest verdict, re-exported so callers
// of this package need no database import to read it.
type RemoteIngestResult = database.RemoteIngestResult

// NewIngestStore binds the durable ingest surface to a migrated database.
func NewIngestStore(db *sql.DB) *IngestStore {
	return &IngestStore{db: db}
}

// IngestEvent persists one inbound event durably. Stored=true on the first
// observation of the key; DedupeDrop=true on every later one. A validation
// refusal (empty key halves) returns the store's error — the subscriber's
// Validate gate already filtered these, so seeing one here is a wiring bug,
// not traffic. The monotone mark advance rides the same call so the
// high-water mark can never lag the newest live event.
func (s *IngestStore) IngestEvent(ctx context.Context, ing IngestedEvent, source string) (RemoteIngestResult, error) {
	if s == nil || s.db == nil {
		return RemoteIngestResult{}, errors.New("bus: ingest store has no database")
	}
	e := database.RemoteEvent{
		SchedulerID: ing.SchedulerID,
		EventID:     ing.EventID,
		Kind:        ing.Kind,
		Project:     ing.Project,
		TickID:      ing.TickID,
		Status:      ing.Status,
		TS:          ing.TS,
		Topic:       ing.Topic,
		Pattern:     ing.Pattern,
		IngestedAt:  ing.IngestedAt.UTC().Format(time.RFC3339Nano),
		Source:      source,
	}
	res, err := database.IngestRemoteEvent(ctx, s.db, &e)
	if err != nil {
		// Autonomy law: never fail the receive path.
		log.Printf("CRIER: remote event store dropped %s/%s: %v", ing.SchedulerID, ing.EventID, err)
		return RemoteIngestResult{}, err
	}
	if res.Stored {
		if merr := database.SetRemoteLastEventID(ctx, s.db, ing.SchedulerID, ing.EventID); merr != nil {
			// The row is stored; a mark hiccup must not unstore it. Log
			// and continue — the next stored event re-attempts advance.
			log.Printf("CRIER: last-event mark advance failed %s/%s: %v", ing.SchedulerID, ing.EventID, merr)
		}
	}
	return res, nil
}

// ReplaySinceGap returns the events to replay for one scheduler:
// everything the store holds above the scheduler's durable LAST-EVENT
// high-water mark. An empty mark (never-seen scheduler) replays the full
// stored history; the caller then re-ingests each through IngestEvent with
// source=database.RemoteSourceReplay — keyed inserts make the whole replay
// idempotent (a second run is a no-op up to dedupe drops).
func (s *IngestStore) ReplaySinceGap(ctx context.Context, schedulerID string, limit int) ([]database.RemoteEvent, string, error) {
	if s == nil || s.db == nil {
		return nil, "", errors.New("bus: replay store has no database")
	}
	last, err := database.RemoteLastEventID(ctx, s.db, schedulerID)
	if err != nil {
		return nil, "", err
	}
	events, err := database.RemoteReplayEvents(ctx, s.db, schedulerID, last, limit)
	if err != nil {
		return nil, last, err
	}
	return events, last, nil
}

// ReplayEvent re-ingests one replayed event. The row's key makes the second
// observation a dedupe drop; when the drop happens the scheduler's durable
// drop counter is bumped so dedupe is COUNTED, not silent (a replay of an
// already-stored interval is expected to be 100% drops).
func (s *IngestStore) ReplayEvent(ctx context.Context, e database.RemoteEvent) (RemoteIngestResult, error) {
	if s == nil || s.db == nil {
		return RemoteIngestResult{}, errors.New("bus: replay store has no database")
	}
	ing := IngestedEvent{
		Envelope: Envelope{
			SchedulerID: e.SchedulerID,
			EventID:     e.EventID,
			Kind:        e.Kind,
			Project:     e.Project,
			TickID:      e.TickID,
			Status:      e.Status,
			TS:          e.TS,
		},
		Topic:   e.Topic,
		Pattern: e.Pattern,
		// REMOTE-006 fallback stamp: the replayed event's own IngestedAt
		// (parsed below when present) takes precedence — this only fires
		// when the stored event carries no ingest stamp. MASK/STAMP
		// entropy, not scheduling time, so clock.Real() here is the
		// wall-clock read the guard permits via the seam.
		IngestedAt: clock.Real().Now().UTC(),
	}
	if e.IngestedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, e.IngestedAt); err == nil {
			ing.IngestedAt = t
		}
	}
	source := e.Source
	switch source {
	case database.RemoteSourceLive:
		// Arrived live first; this replay re-observes the stored row.
		source = database.RemoteSourceReplayedLive
	case "":
		source = database.RemoteSourceReplay
	default:
		// replay / replayed-live / anything else: stays itself.
	}
	res, err := database.IngestRemoteEvent(ctx, s.db, &database.RemoteEvent{
		SchedulerID: e.SchedulerID,
		EventID:     e.EventID,
		Kind:        e.Kind,
		Project:     e.Project,
		TickID:      e.TickID,
		Status:      e.Status,
		TS:          e.TS,
		Topic:       e.Topic,
		Pattern:     e.Pattern,
		IngestedAt:  ing.IngestedAt.Format(time.RFC3339Nano),
		Source:      source,
	})
	if err != nil {
		log.Printf("CRIER: replay store dropped %s/%s: %v", e.SchedulerID, e.EventID, err)
		return res, err
	}
	if res.DedupeDrop {
		if berr := database.BumpRemoteDedupeDrops(ctx, s.db, e.SchedulerID, 1); berr != nil {
			log.Printf("CRIER: dedupe-drop bump failed %s/%s: %v", e.SchedulerID, e.EventID, berr)
		}
	}
	// THE GAP-CLOSE: replaying an event means it has now been SEEN — the
	// mark must cover it whether the replay STORED it (a genuinely new row)
	// or DROPPED it as a duplicate (the common case: the spool already held
	// the row while the mark lagged). A mark left behind the replayed tail
	// would re-deliver the same interval on every reconnect, forever.
	if merr := database.SetRemoteLastEventID(ctx, s.db, e.SchedulerID, e.EventID); merr != nil {
		log.Printf("CRIER: replay mark advance failed %s/%s: %v", e.SchedulerID, e.EventID, merr)
	}
	return res, nil
}
