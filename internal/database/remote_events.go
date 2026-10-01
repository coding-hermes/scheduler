package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// REMOTE-006 (docs/remote-spec.md §5, partition contract): the DURABLE store
// for inbound federation events.
//
//   - Idempotency: (scheduler_id, event_id) is the spec's idempotency key;
//     the v58 table carries it as the composite PRIMARY KEY, so a duplicate
//     insert is physically impossible. IngestRemoteEvent writes with
//     `ON CONFLICT DO NOTHING` and reports the duplicate as a COUNTED dedupe
//     drop — never an error, and never a second row.
//   - Ordering: there is deliberately NO global order (§5: "Cross-box ordering
//     is per-scheduler + correlation id"). The only ordering state kept is the
//     per-scheduler LAST-SEEN EVENT high-water mark (RemoteLastEventID /
//     SetRemoteLastEventID over remote_events_stats), consulted by the replay
//     policy on (re)subscribe.
//   - Replay: RemoteReplayEvents selects everything above the mark for one
//     scheduler; because inserts are keyed and ON CONFLICT DO NOTHING, a
//     replay is idempotent by construction — running it twice cannot add a
//     row, it can only re-drop.

// RemoteEventSource enumerates the remote_events.source vocabulary: how the
// stored row arrived.
//
//	live           — delivered by the subscriber's normal receive path
//	replay         — the first replay that landed the row
//	replayed-live  — the event arrived live FIRST and replay re-observed it
//	                 (row already present; the replay dropped it — kept so
//	                 an id never carries two vocabularies)
const (
	RemoteSourceLive         = "live"
	RemoteSourceReplay       = "replay"
	RemoteSourceReplayedLive = "replayed-live"
)

// RemoteEvent is one inbound peer event as stored durably. The envelope
// members mirror bus.Envelope; the receive-side facts (topic/pattern/
// ingested_at/source) are this store's own.
type RemoteEvent struct {
	SchedulerID string `json:"scheduler_id"`
	EventID     string `json:"event_id"`
	Kind        string `json:"kind"`
	Project     string `json:"project"`
	TickID      string `json:"tick_id"`
	Status      string `json:"status"`
	// TS is the publisher's original event timestamp (the envelope's ts).
	TS string `json:"ts"`
	// Topic is the literal published topic the frame named; Pattern is the
	// subscription pattern the frame arrived on ("" when the event did not
	// come from a subscription frame, e.g. a test-written row).
	Topic   string `json:"topic"`
	Pattern string `json:"pattern"`
	// IngestedAt is the receive-side stamp (UTC RFC3339) — the primary's
	// receive time, the spec's §8 staleness recommendation.
	IngestedAt string `json:"ingested_at"`
	// Source is one of the RemoteSource* vocabulary values.
	Source string `json:"source"`
}

// ErrRemoteEventInvalid reports an ingest request the store refuses on its
// face: an empty scheduler id or an empty event id would corrupt the
// idempotency key (both halves must be present for dedupe to mean anything).
var ErrRemoteEventInvalid = errors.New("remote event: scheduler_id and event_id are required")

// RemoteIngestResult reports what one ingest did.
type RemoteIngestResult struct {
	// Stored is true when the event inserted a new row (first observation).
	Stored bool
	// DedupeDrop is true when the key already existed — a COUNTED dedupe
	// drop, not an error (spec §5: "the ingest side drops a duplicate key").
	DedupeDrop bool
}

// IngestRemoteEvent stores one inbound event keyed by
// (scheduler_id, event_id). The FIRST observation inserts (Stored=true);
// every later observation of the same key is a dedupe drop (DedupeDrop=true,
// nil error) — a replayed spool can never double-apply (§5).
func IngestRemoteEvent(ctx context.Context, db *sql.DB, e *RemoteEvent) (RemoteIngestResult, error) {
	if strings.TrimSpace(e.SchedulerID) == "" || strings.TrimSpace(e.EventID) == "" {
		return RemoteIngestResult{}, ErrRemoteEventInvalid
	}
	if e.Source == "" {
		e.Source = RemoteSourceLive
	}
	const q = `INSERT INTO remote_events
    (scheduler_id, event_id, kind, project, tick_id, status, ts, topic, pattern, ingested_at, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(scheduler_id, event_id) DO NOTHING`
	res, err := db.ExecContext(ctx, q,
		e.SchedulerID, e.EventID, e.Kind, e.Project, e.TickID, e.Status,
		e.TS, e.Topic, e.Pattern, e.IngestedAt, e.Source)
	if err != nil {
		return RemoteIngestResult{}, fmt.Errorf("ingest remote event %s/%s: %w",
			e.SchedulerID, e.EventID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return RemoteIngestResult{}, fmt.Errorf("ingest remote event rows affected %s/%s: %w",
			e.SchedulerID, e.EventID, err)
	}
	if n == 0 {
		return RemoteIngestResult{DedupeDrop: true}, nil
	}
	return RemoteIngestResult{Stored: true}, nil
}

// CountRemoteEventDedupeDrops returns a scheduler's cumulative dedupe-drop
// counter ("" = every scheduler). The counter is durable in
// remote_events_stats and bumped by the ingest wrapper below — a stat read
// must never cost a table scan on a busy receive path.
func CountRemoteEventDedupeDrops(ctx context.Context, db *sql.DB, schedulerID string) (int, error) {
	q := `SELECT COALESCE(SUM(dedupe_drops), 0) FROM remote_events_stats`
	args := []any{}
	if schedulerID != "" {
		q += ` WHERE scheduler_id = ?`
		args = append(args, schedulerID)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count remote dedupe drops: %w", err)
	}
	return n, nil
}

// BumpRemoteDedupeDrops adds n to a scheduler's durable dedupe-drop counter.
// Called by the bus ingest wrapper whenever IngestRemoteEvent reports a
// dedupe drop, so the counter is the store-side truth about duplicates.
func BumpRemoteDedupeDrops(ctx context.Context, db *sql.DB, schedulerID string, n int64) error {
	if strings.TrimSpace(schedulerID) == "" {
		return errors.New("bump remote dedupe drops: scheduler_id required")
	}
	const q = `INSERT INTO remote_events_stats (scheduler_id, dedupe_drops) VALUES (?, ?)
ON CONFLICT(scheduler_id) DO UPDATE SET dedupe_drops = dedupe_drops + excluded.dedupe_drops`
	if _, err := db.ExecContext(ctx, q, schedulerID, n); err != nil {
		return fmt.Errorf("bump remote dedupe drops %q: %w", schedulerID, err)
	}
	return nil
}

// RemoteLastEventID returns the per-scheduler LAST-SEEN EVENT high-water
// mark (the greatest event_id observed, by counter order). Returns "" when
// the scheduler has never been seen — the replay-from-scratch condition.
func RemoteLastEventID(ctx context.Context, db *sql.DB, schedulerID string) (string, error) {
	if strings.TrimSpace(schedulerID) == "" {
		return "", errors.New("remote last event id: scheduler_id required")
	}
	var last string
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(last_event_id, '') FROM remote_events_stats WHERE scheduler_id = ?`,
		schedulerID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("remote last event id %q: %w", schedulerID, err)
	}
	return last, nil
}

// SetRemoteLastEventID advances a scheduler's high-water mark. THE MONOTONE
// LAW: the mark only moves FORWARD (by the max counter arm) — a lagging
// write (a stale replay racing a live event, an out-of-order delivery) can
// never pull it backward, so a later reconnect cannot re-deliver an interval
// the mark already covers.
func SetRemoteLastEventID(ctx context.Context, db *sql.DB, schedulerID, eventID string) error {
	if strings.TrimSpace(schedulerID) == "" {
		return errors.New("set remote last event id: scheduler_id required")
	}
	// Read-modify-write with the monotone comparison done in Go. Safe under
	// this daemon's concurrency shape: one ingest loop per scheduler id (the
	// subscriber's serial handleFrame → ingest path), and remote_events_stats
	// is keyed per scheduler.
	cur, err := RemoteLastEventID(ctx, db, schedulerID)
	if err != nil {
		return err
	}
	if !remoteCounterLess(cur, eventID) {
		return nil // the mark already covers this id — never move backward
	}
	const q = `INSERT INTO remote_events_stats (scheduler_id, last_event_id) VALUES (?, ?)
ON CONFLICT(scheduler_id) DO UPDATE SET last_event_id = excluded.last_event_id`
	if _, err := db.ExecContext(ctx, q, schedulerID, eventID); err != nil {
		return fmt.Errorf("set remote last event id %q: %w", schedulerID, err)
	}
	return nil
}

// remoteCounterLess orders two event ids by the spec's §8 mint shape
// ("<scheduler_id>-<20-digit counter>", counter-monotonic within one
// scheduler). Non-counter ids (foreign or hand-built) have no defined order:
// they never move an existing counter-form mark, but they MAY establish a
// first mark so a scheduler that only ever emits non-counter ids still
// records a floor. With both non-counter, the later write wins (arrival
// order — the only order such ids have).
func remoteCounterLess(cur, next string) bool {
	if cur == "" {
		return true
	}
	cCur, okCur := parseRemoteCounter(cur)
	cNext, okNext := parseRemoteCounter(next)
	switch {
	case okCur && okNext:
		return cCur < cNext
	case okCur && !okNext:
		return false // a counter mark is never moved by a non-counter id
	case !okCur && okNext:
		return true // establish the counter ordering
	default:
		return true // both non-counter: arrival order wins
	}
}

// parseRemoteCounter decodes the 20-digit tail of a counter-minted event id
// ("<anything>-<20 digits>"). ok=false for ids the mint format does not
// explain — bus.ParseEventID's contract, mirrored locally so the database
// package keeps no import on the bus package.
func parseRemoteCounter(eventID string) (uint64, bool) {
	const digits = 20
	if len(eventID) <= digits+1 || eventID[len(eventID)-digits-1] != '-' {
		return 0, false
	}
	tail := eventID[len(eventID)-digits:]
	var v uint64
	for i := 0; i < digits; i++ {
		c := tail[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}

// RemoteReplayEvents returns the events to replay for one scheduler whose
// stored high-water mark is lastEventID ("": full history — the never-seen
// scheduler replays everything this store holds). Ordering is ingested_at
// then event_id — replay must be DETERMINISTIC so a second replay observes
// the same sequence and stays a no-op. The caller re-ingests the returned
// events; keyed inserts make the re-ingest idempotent.
func RemoteReplayEvents(ctx context.Context, db *sql.DB, schedulerID, lastEventID string, limit int) ([]RemoteEvent, error) {
	if strings.TrimSpace(schedulerID) == "" {
		return nil, errors.New("remote replay events: scheduler_id required")
	}
	q := `SELECT scheduler_id, event_id, kind, project, tick_id, status, ts, topic, pattern, ingested_at, source
FROM remote_events WHERE scheduler_id = ?`
	args := []any{schedulerID}
	if lastEventID != "" {
		// Membership by the counter tail: event ids are
		// "<scheduler_id>-<20-digit counter>", zero-padded, so the
		// LEXICOGRAPHIC order on the fixed-width tail IS counter order.
		// A mark the comparison cannot explain (non-counter id) selects
		// nothing — conservative: replay may re-deliver, ingest drops.
		if _, ok := parseRemoteCounter(lastEventID); ok {
			q += ` AND length(event_id) >= 21
			   AND substr(event_id, -21, 1) = '-'
			   AND substr(event_id, -20) > ?`
			args = append(args, lastEventID[len(lastEventID)-20:])
		}
	}
	q += ` ORDER BY ingested_at ASC, event_id ASC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("remote replay events %q: %w", schedulerID, err)
	}
	defer rows.Close()
	out := []RemoteEvent{}
	for rows.Next() {
		var e RemoteEvent
		if err := rows.Scan(&e.SchedulerID, &e.EventID, &e.Kind, &e.Project, &e.TickID,
			&e.Status, &e.TS, &e.Topic, &e.Pattern, &e.IngestedAt, &e.Source); err != nil {
			return nil, fmt.Errorf("scan remote event row: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate remote event rows: %w", err)
	}
	return out, nil
}

// RemoteEventCount counts stored events for one scheduler ("" = all), the
// dashboard's per-peer activity fact.
func RemoteEventCount(ctx context.Context, db *sql.DB, schedulerID string) (int, error) {
	q := `SELECT COUNT(*) FROM remote_events`
	args := []any{}
	if schedulerID != "" {
		q += ` WHERE scheduler_id = ?`
		args = append(args, schedulerID)
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count remote events: %w", err)
	}
	return n, nil
}

// RemoteLastEvent returns the scheduler's most recently INGESTED event (by
// receive time, then event id) — the dashboard's "last-seen event" cell.
// sql.ErrNoRows means this scheduler has no stored events yet; the caller
// decides how to render that.
func RemoteLastEvent(ctx context.Context, db *sql.DB, schedulerID string) (*RemoteEvent, error) {
	row := db.QueryRowContext(ctx, `SELECT scheduler_id, event_id, kind, project, tick_id, status, ts, topic, pattern, ingested_at, source
FROM remote_events WHERE scheduler_id = ? ORDER BY ingested_at DESC, event_id DESC LIMIT 1`, schedulerID)
	e := &RemoteEvent{}
	err := row.Scan(&e.SchedulerID, &e.EventID, &e.Kind, &e.Project, &e.TickID,
		&e.Status, &e.TS, &e.Topic, &e.Pattern, &e.IngestedAt, &e.Source)
	if err != nil {
		return nil, err // sql.ErrNoRows passes through — caller decides
	}
	return e, nil
}

// RemotePeerLastEvent is one peer's dashboard row: the registry identity +
// its last-seen event and freshness (spec §5 + §2 rendering law: STALE with a
// timestamp, never "down").
type RemotePeerLastEvent struct {
	// Peer is the registry row (identity, url, version, last_contact).
	Peer Peer `json:"peer"`
	// LastEvent is the most recently ingested event from this scheduler.
	// Nil = no event from this peer has ever been stored.
	LastEvent *RemoteEvent `json:"last_event,omitempty"`
	// LastEventID is the peer's replay high-water mark ("" = never seen).
	LastEventID string `json:"last_event_id"`
	// Stale is THE freshness verdict (IsPeerStale over the caller's window).
	Stale bool `json:"stale"`
	// EventCount is how many of this peer's events the store holds.
	EventCount int `json:"event_count"`
}

// RemotePeersWithLastEvent assembles the dashboard's Remote section in ONE
// pass: every registered peer with its last-seen event + staleness (the
// registry's stale-window semantics, reused verbatim via IsPeerStale —
// stale ≠ down). Freshness is evaluated at READ time, exactly as the
// registry's own surfaces do.
func RemotePeersWithLastEvent(ctx context.Context, db *sql.DB, windowSeconds int, now func() time.Time) ([]RemotePeerLastEvent, error) {
	peers, err := ListPeers(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("remote peers with last event: %w", err)
	}
	out := make([]RemotePeerLastEvent, 0, len(peers))
	for i := range peers {
		p := peers[i]
		entry := RemotePeerLastEvent{Peer: p}
		entry.Stale = IsPeerStale(p.LastContact, windowSeconds, now)
		if ev, err := RemoteLastEvent(ctx, db, p.ID); err == nil {
			entry.LastEvent = ev
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("remote last event for peer %q: %w", p.ID, err)
		}
		if n, err := RemoteEventCount(ctx, db, p.ID); err != nil {
			return nil, fmt.Errorf("remote event count for peer %q: %w", p.ID, err)
		} else {
			entry.EventCount = n
		}
		last, err := RemoteLastEventID(ctx, db, p.ID)
		if err != nil {
			return nil, err
		}
		entry.LastEventID = last
		out = append(out, entry)
	}
	return out, nil
}
