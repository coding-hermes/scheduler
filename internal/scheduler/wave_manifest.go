package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// ── S12 §9.3 (SCHED-GAP-110): wave manifest ingest ───────────────────────
//
// The foreman's own record of a wave tick lives at
// <workdir>/.coding-hermes/waves/<tick_id>.json. The scheduler reads it at
// tick completion to fill ticks.worker_count and one tick_workers
// attribution row per dispatched worker. Everything here is BEST-EFFORT and
// deliberately fail-safe (§9.3 ingestion table): a missing manifest is a
// normal serial tick (no event), a malformed/oversize/mismatched manifest
// costs exactly one WARN event and nothing else — it can never fail the
// tick, change its status/outcome/cost, or alter the completion callback's
// return value. Money invariant (W4, §5.2): tick_workers.cost_usd is
// attribution from the manifest ONLY; ticks.cost_usd is never touched here
// and remains the tick's single cost figure.

// waveManifestMaxBytes bounds the manifest read (S12 §9.3: file > 64 KiB is
// not parsed, WARN). Enforced twice: os.Stat before opening, and an
// io.LimitReader cap so a file that grows between the two checks still
// cannot drag an unbounded payload into memory.
const waveManifestMaxBytes = 64 << 10

// waveManifestMaxWorkers bounds the number of worker rows ingested from one
// manifest (S12 §9.3: workers > 8 → first 8 ingested, WARN). The foreman's
// own wave cap is 3; 8 is the tolerant ceiling for unusual merges.
const waveManifestMaxWorkers = 8

// errWaveManifestAbsent is the sentinel for "no manifest file" — the normal
// serial tick. The caller emits NO event for this case (a serial tick is
// not a warning); every other parse failure is worth one WARN.
var errWaveManifestAbsent = errors.New("wave manifest absent")

// WaveManifest is the foreman-written wave record (S12 §9.3 contract).
// Unknown keys are ignored on parse (tolerant ingest); TruncatedWorkers is
// not part of the file format — it carries the parse-time truncation signal
// (workers beyond the 8-row cap were dropped) so ingestWaveManifest can
// turn it into exactly one WARN event.
type WaveManifest struct {
	TickID     string       `json:"tick_id"`
	Project    string       `json:"project"`
	StartedAt  string       `json:"started_at"`
	FinishedAt string       `json:"finished_at"`
	Workers    []WaveWorker `json:"workers"`

	// TruncatedWorkers counts worker entries dropped by the
	// waveManifestMaxWorkers cap. json:"-" keeps it out of the wire format.
	TruncatedWorkers int `json:"-"`
}

// WaveWorker is one dispatched worker as reported by the foreman. Judge and
// Merge are normalized on parse to the tick_workers CHECK vocabularies so a
// stray manifest value can never fail an INSERT (S12 §13). RawJudge and
// RawMerge retain the pre-normalization value verbatim so the ingest path
// can detect vocabulary drift (SCHED-GAP-218) and surface it as a single
// MEDIUM-severity event per manifest.
type WaveWorker struct {
	TaskID    string  `json:"task_id"`
	Branch    string  `json:"branch"`
	Worktree  string  `json:"worktree"`
	CommitSHA string  `json:"commit_sha"`
	Judge     string  `json:"judge"`
	Merge     string  `json:"merge"`
	CostUSD   float64 `json:"cost_usd"`
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`

	// RawJudge / RawMerge are the original (pre-normalization) judge /
	// merge strings as read from the manifest. They are populated by
	// parseWaveManifest, are never written to the database, and are
	// only used to surface vocabulary drift (SCHED-GAP-218) as a
	// single loud event per manifest.
	RawJudge string `json:"-"`
	RawMerge string `json:"-"`
}

// waveManifestPath returns <workdir>/.coding-hermes/waves/<tick_id>.json —
// the foreman-owned location (S12 §9.3; workers never write board state).
func waveManifestPath(workdir, tickID string) string {
	return filepath.Join(workdir, ".coding-hermes", "waves", tickID+".json")
}

// normalizeWaveJudge maps a manifest judge value onto the tick_workers
// vocabulary ('pass'|'fail'|'withdrawn'|'unknown'). Contract (SCHED-GAP-218):
//
//	(1) Leading and trailing ASCII whitespace is trimmed.
//	(2) The first whitespace-separated token is the candidate; the rest of
//	    the string (the "evidence" tail) is ignored.
//	(3) The token is matched case-insensitively against
//	    {pass, fail, withdrawn, unknown}.
//	(4) A match returns the canonical lowercase form; anything else
//	    degrades to 'unknown' rather than failing the CHECK constraint
//	    (S12 §13: never let a manifest value fail an INSERT).
//
// Examples:
//
//	"PASS 94b82015"        -> "pass"
//	"pass"                 -> "pass"
//	" Pass "               -> "pass"
//	"FAIL <sha>"           -> "fail"
//	"withdrawn <reason>"   -> "withdrawn"
//	"garbage <sha>"        -> "unknown"
//	""                     -> "unknown"
func normalizeWaveJudge(v string) string {
	return matchWaveVocab(v, database.TickWorkerJudgePass,
		database.TickWorkerJudgeFail, database.TickWorkerJudgeWithdrawn,
		database.TickWorkerJudgeUnknown, database.TickWorkerJudgeUnknown)
}

// normalizeWaveMerge maps a manifest merge value onto the tick_workers
// vocabulary ('merged'|'conflict'|'preserved'|'pending'). Contract
// (SCHED-GAP-218) mirrors normalizeWaveJudge: case-insensitive
// leading-token match against the four canonical tokens, trailing
// evidence ignored, anything else degrades to 'pending'.
//
// Examples:
//
//	"already merged as 4ceb0c5" -> "merged"
//	"merged"                    -> "merged"
//	" MERGED "                  -> "merged"
//	"conflict ..."              -> "conflict"
//	"preserved as evidence"     -> "preserved"
//	"garbage <sha>"             -> "pending"
//	""                          -> "pending"
func normalizeWaveMerge(v string) string {
	return matchWaveVocab(v, database.TickWorkerMergeMerged,
		database.TickWorkerMergeConflict, database.TickWorkerMergePreserved,
		database.TickWorkerMergePending, database.TickWorkerMergePending)
}

// matchWaveVocab is the shared shape behind normalizeWaveJudge /
// normalizeWaveMerge: trim, then look for the first canonical token
// in the leading whitespace-separated tokens (case-insensitive).
// Anything still outside the allowed set degrades to def. We try the
// first two tokens because common producer phrasings prepend a
// connector ("already merged as <sha>", "still in merged state", etc.)
// where the canonical token sits at position 1, not 0. The "evidence
// tail" past the canonical token (the "<sha>" portion) is ignored.
// Allowed tokens (case-insensitive):
//
//	judge:  pass, fail, withdrawn, unknown
//	merge:  merged, conflict, preserved, pending
//
// The five form-name args (p, f, w, u, def) map the matched token to
// the canonical lowercase string used in the DB.
func matchWaveVocab(v, p, f, w, u, def string) string {
	for _, tok := range firstNVocabTokens(v, 2) {
		switch tok {
		case "pass":
			return p
		case "fail":
			return f
		case "withdrawn":
			return w
		case "unknown":
			return u
		case "merged":
			return p
		case "conflict":
			return f
		case "preserved":
			return w
		case "pending":
			return u
		}
	}
	return def
}

// firstNVocabTokens returns up to n lower-cased whitespace-separated
// tokens from s, skipping empty tokens. Returns an empty slice for
// the empty/whitespace input.
func firstNVocabTokens(s string, n int) []string {
	s = strings.TrimSpace(s)
	if s == "" || n <= 0 {
		return nil
	}
	fields := strings.Fields(s)
	if len(fields) > n {
		fields = fields[:n]
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(f))
	}
	return out
}

// detectWaveVocabDrift reports which workers in the manifest carried a
// raw judge/merge value the widened matchWaveVocab grammar still cannot
// map onto the canonical vocabulary, plus a single sample of each
// drifted value. Used by ingestWaveManifest to emit exactly one
// MEDIUM-severity event per manifest (SCHED-GAP-218) so a future
// writer/consumer mismatch is queryable, never silent. The function is
// pure (no I/O) so the unit tests can drive it directly.
func detectWaveVocabDrift(m *WaveManifest) (ids []string, judgeDrift bool, mergeDrift bool, sampleJudge string, sampleMerge string) {
	for _, w := range m.Workers {
		j, f, jSample, mSample := driftOne(w.RawJudge, w.RawMerge)
		if j || f {
			ids = append(ids, w.TaskID)
		}
		if j {
			judgeDrift = true
			if sampleJudge == "" {
				sampleJudge = jSample
			}
		}
		if f {
			mergeDrift = true
			if sampleMerge == "" {
				sampleMerge = mSample
			}
		}
	}
	return ids, judgeDrift, mergeDrift, sampleJudge, sampleMerge
}

// driftOne applies the same leading-token rule as matchWaveVocab and
// reports whether the raw values are not in the canonical vocab. A
// raw value is considered a drift ONLY when neither its first nor
// its second whitespace-separated token (case-insensitive) is in the
// canonical vocab, AND the second position is only consulted when
// the first token is a known connector word (so a bare "<sha>" or
// "abc1234" tail can never accidentally match by coincidence). An
// empty value is the documented "unreported verdict" path (see
// waveWorkerTerminalState), not drift.
func driftOne(rawJudge, rawMerge string) (judgeDrift bool, mergeDrift bool, sampleJudge, sampleMerge string) {
	if !vocabHitStrict(rawJudge, judgeTokens) && hasAnyNonEmptyToken(rawJudge) {
		judgeDrift = true
		sampleJudge = rawJudge
	}
	if !vocabHitStrict(rawMerge, mergeTokens) && hasAnyNonEmptyToken(rawMerge) {
		mergeDrift = true
		sampleMerge = rawMerge
	}
	return judgeDrift, mergeDrift, sampleJudge, sampleMerge
}

// waveDriftConnectors is the small allow-list of leading words that
// grant the second token a chance to be canonical. Producers that
// need new connectors add them here with a one-line comment naming
// the producer / evidence tick. Kept tiny on purpose — a wide
// allow-list would silently mask real drift; a missing connector
// produces a single loud MEDIUM event the next tick, which is
// cheaper than misclassifying "<sha>" as a canonical token.
var waveDriftConnectors = map[string]struct{}{
	"already": {}, // crier-2026-09-19-19-37-43: "already merged as <sha>"
	"still":   {}, // pending → "still pending" / merged → "still merged"
	"now":     {}, // "now merged", "now withdrawn"
	"as":      {}, // "as merged", "as withdrawn"
	"is":      {}, // "is merged", "is preserved"
	"now-":    {}, // rare: "now-merged" treated as a single token
}

// judgeTokens / mergeTokens are the canonical lowercase vocabularies
// the drift detector consults (mirrors matchWaveVocab's switch).
var (
	judgeTokens = []string{"pass", "fail", "withdrawn", "unknown"}
	mergeTokens = []string{"merged", "conflict", "preserved", "pending"}
)

// vocabHitStrict returns true iff either the first or (first
// connector + second canonical) shape matches vocab. The second
// position is only consulted when the first token is in
// waveDriftConnectors — that prevents a bare hex/sha tail from
// accidentally satisfying the canonical check.
func vocabHitStrict(raw string, vocab []string) bool {
	toks := firstNVocabTokens(raw, 2)
	if len(toks) == 0 {
		return false
	}
	for _, v := range vocab {
		if toks[0] == v {
			return true
		}
	}
	if len(toks) >= 2 {
		if _, ok := waveDriftConnectors[toks[0]]; ok {
			for _, v := range vocab {
				if toks[1] == v {
					return true
				}
			}
		}
	}
	return false
}

// hasAnyNonEmptyToken is true iff s has at least one non-whitespace
// character (used to distinguish "unreported verdict" from "drift").
func hasAnyNonEmptyToken(s string) bool {
	return strings.TrimSpace(s) != ""
}

// parseWaveManifest reads and validates the wave manifest for tickID.
//
// Behavior (S12 §9.3 ingestion table):
//   - file missing            → (nil, errWaveManifestAbsent) — caller emits NO event
//   - file > waveManifestMaxBytes → (nil, error) — caller emits one WARN
//   - malformed JSON          → (nil, error) — caller emits one WARN
//   - tick_id mismatch        → (nil, error) — caller emits one WARN, nothing ingested
//   - workers > 8             → first 8 kept, m.TruncatedWorkers set — caller emits one WARN
//
// Unknown JSON keys are ignored (json.Unmarshal default). The returned
// manifest always carries normalized judge/merge values; the
// pre-normalization raw values are preserved on each worker as
// RawJudge/RawMerge so the ingest path can surface vocabulary drift
// (SCHED-GAP-218) as a single loud event per manifest, never silently.
func parseWaveManifest(path, tickID string) (*WaveManifest, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", errWaveManifestAbsent, path)
		}
		return nil, fmt.Errorf("wave manifest stat %s: %w", path, err)
	}
	if st.Size() > waveManifestMaxBytes {
		return nil, fmt.Errorf("wave manifest %s oversized (%d bytes > %d)", path, st.Size(), waveManifestMaxBytes)
	}

	// Hard cap the read as well: the file may grow between Stat and ReadFile.
	f, err := os.Open(path) //nolint:gosec // path is constructed from scheduler-owned ids, not user input
	if err != nil {
		return nil, fmt.Errorf("wave manifest open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only handle; Close failure is irrelevant

	raw, err := io.ReadAll(io.LimitReader(f, waveManifestMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("wave manifest read %s: %w", path, err)
	}
	if len(raw) > waveManifestMaxBytes {
		return nil, fmt.Errorf("wave manifest %s oversized (read %d bytes > %d)", path, len(raw), waveManifestMaxBytes)
	}

	var m WaveManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("wave manifest parse %s: %w", path, err)
	}
	if m.TickID != tickID {
		return nil, fmt.Errorf("wave manifest %s tick_id mismatch: manifest %q, completing tick %q", path, m.TickID, tickID)
	}

	// Bound the worker list; remember how many were dropped so ingest can
	// surface exactly one WARN for the truncation.
	if len(m.Workers) > waveManifestMaxWorkers {
		m.TruncatedWorkers = len(m.Workers) - waveManifestMaxWorkers
		m.Workers = m.Workers[:waveManifestMaxWorkers]
	}
	for i := range m.Workers {
		// Preserve the pre-normalization raw values so the ingest path
		// can detect vocabulary drift (SCHED-GAP-218) and emit one
		// loud event per manifest, not silently discard unrecognized
		// shapes.
		m.Workers[i].RawJudge = m.Workers[i].Judge
		m.Workers[i].RawMerge = m.Workers[i].Merge
		m.Workers[i].Judge = normalizeWaveJudge(m.Workers[i].Judge)
		m.Workers[i].Merge = normalizeWaveMerge(m.Workers[i].Merge)
	}
	return &m, nil
}

// waveWorkerTerminalState picks the tick_workers.state for an ingested
// worker row. DESIGN CHOICE (documented per brief): the manifest is read at
// tick completion, so a worker carrying a terminal verdict — judge in
// pass/fail/withdrawn OR merge in merged/conflict/preserved — is recorded
// 'done'. Workers with no verdict stay 'running': per S12 §10.3 their
// terminal transition belongs to the worker-report/reaper/recovery phases
// (SCHED-GAP-113/114), not to ingestion guessing on their behalf.
func waveWorkerTerminalState(w WaveWorker) string {
	if w.Judge != database.TickWorkerJudgeUnknown ||
		w.Merge != database.TickWorkerMergePending {
		return database.TickWorkerStateDone
	}
	return database.TickWorkerStateRunning
}

// waveWarnEvent logs the single WARN-tier event for a manifest problem.
// The events table has no WARN severity (CHECK: CRITICAL/HIGH/MEDIUM/LOW/
// INFO) — MEDIUM carries demoted warnings, the SCHED-GAP-061 convention
// used wherever a spec says "WARN event".
func waveWarnEvent(ctx context.Context, db *sql.DB, tickID, path, message string) {
	_ = database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityMedium,
		Component: "wave",
		Message:   fmt.Sprintf("tick %s: %s (%s)", tickID, message, path),
	})
}

// ingestWaveManifest ingests the foreman's wave manifest for a just-ended
// tick into ticks.worker_count + one tick_workers row per worker, all in
// ONE transaction so the count and the rows can never disagree (§5.2).
//
// Contract (best-effort, never fails the tick):
//   - absent manifest          → (0, nil), NO event (serial tick is normal)
//   - any other parse problem  → (0, nil) + exactly one WARN event naming
//     the tick id and path; nothing ingested
//   - workers > 8              → first 8 ingested + one WARN event
//   - success                  → worker rows inserted, worker_count set,
//     (ingested count, nil)
//
// W4 money invariant: tick_workers.cost_usd stores the manifest's
// attribution figure verbatim and ticks.cost_usd is NEVER modified here.
//
// The returned error is reserved for scheduler-side infrastructure faults
// (e.g. the transaction cannot begin); the completion-path caller ignores
// it by design — manifest problems are already surfaced as WARN events.
func ingestWaveManifest(ctx context.Context, db *sql.DB, workdir, project, tickID string) (int, error) {
	path := waveManifestPath(workdir, tickID)
	m, err := parseWaveManifest(path, tickID)
	if err != nil {
		if errors.Is(err, errWaveManifestAbsent) {
			return 0, nil // serial tick — no event
		}
		waveWarnEvent(ctx, db, tickID, path, err.Error())
		return 0, nil
	}

	// SCHED-GAP-218: detect vocabulary drift BEFORE opening the
	// transaction. waveWarnEvent opens its own SQLite connection
	// (database/sql pool), and the connection budget is
	// SetMaxOpenConns(1) with busy_timeout=5000; emitting the drift
	// event from inside a tx would block the tx's own worker
	// INSERTs and deadlock. One event per manifest, naming the raw
	// values and the tick_id — never silent, never per-worker.
	if driftIDs, judgeDrift, mergeDrift, sampleJudge, sampleMerge := detectWaveVocabDrift(m); len(driftIDs) > 0 {
		extra := ""
		if len(driftIDs) > 5 {
			extra = fmt.Sprintf(" and %d more", len(driftIDs)-5)
		}
		waveWarnEvent(ctx, db, tickID, path,
			fmt.Sprintf("vocab drift: %d worker(s) — judge_drift=%v sample=%q merge_drift=%v sample=%q (ids: %s%s)",
				len(driftIDs), judgeDrift, sampleJudge, mergeDrift, sampleMerge,
				strings.Join(driftIDs[:min(5, len(driftIDs))], ", "), extra))
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("wave manifest ingest %s: begin tx: %w", tickID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op on commit; the only error path is already returned

	res, err := tx.ExecContext(ctx,
		`UPDATE ticks SET worker_count = ? WHERE id = ?`, len(m.Workers), tickID)
	if err != nil {
		return 0, fmt.Errorf("wave manifest ingest %s: set worker_count: %w", tickID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("wave manifest ingest %s: worker_count rows affected: %w", tickID, err)
	}
	if n == 0 {
		// Terminal tick row missing: nothing to attribute to. Roll back and
		// surface as a WARN — the manifest exists but cannot be applied.
		waveWarnEvent(ctx, db, tickID, path, "tick row not found at ingest — nothing written")
		return 0, nil
	}

	now := clock.FromContext(ctx).Now().UTC().Format(time.RFC3339)
	const q = `INSERT INTO tick_workers
(tick_id, task_id, branch, worktree, commit_sha, judge, merge, state, cost_usd, tokens_in, tokens_out, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	for _, w := range m.Workers {
		// Judge/Merge were normalized at parse; the state derives from the
		// normalized verdicts. All values are bound parameters — manifest
		// data is never interpolated into SQL (S12 §13).
		if _, err := tx.ExecContext(ctx, q,
			tickID, w.TaskID, w.Branch, w.Worktree, w.CommitSHA,
			w.Judge, w.Merge, waveWorkerTerminalState(w),
			w.CostUSD, w.TokensIn, w.TokensOut, now, now); err != nil {
			return 0, fmt.Errorf("wave manifest ingest %s: insert worker %q: %w", tickID, w.TaskID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("wave manifest ingest %s: commit: %w", tickID, err)
	}

	if m.TruncatedWorkers > 0 {
		waveWarnEvent(ctx, db, tickID, path,
			fmt.Sprintf("manifest listed %d workers — first %d ingested, %d dropped (bounded write)",
				len(m.Workers)+m.TruncatedWorkers, len(m.Workers), m.TruncatedWorkers))
	}
	_ = project // reserved for future attribution; the tick row already names the project
	return len(m.Workers), nil
}

// waveManifestUnfinished reports whether the manifest at path is a
// recovery candidate: it parses cleanly (bounded, tolerant — any parse
// problem, missing file, or oversize payload yields false, the
// conservative answer) AND finished_at is empty, meaning the foreman died
// before closing its wave. Consumed by the wave-recovery phase
// (SCHED-GAP-114); per §9.3, a manifest with non-empty finished_at was
// closed by the foreman and is never a candidate.
func waveManifestUnfinished(path string) bool {
	// The expected tick id is the filename stem (waves/<tick_id>.json) —
	// parseWaveManifest's mismatch check then doubles as a self-consistency
	// guard: a manifest whose tick_id disagrees with its own filename is
	// not a trustworthy recovery candidate.
	m, err := parseWaveManifest(path, waveTickIDFromPath(path))
	if err != nil || m == nil {
		return false
	}
	return m.FinishedAt == ""
}

// waveTickIDFromPath extracts <tick_id> from a .../waves/<tick_id>.json
// path. Empty string when the stem carries no usable id.
func waveTickIDFromPath(path string) string {
	stem := filepath.Base(path)
	return strings.TrimSuffix(stem, filepath.Ext(stem))
}
