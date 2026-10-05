package scheduler

// SCHED-GAP-1710 — which lanes are executed by a NAMED AGENT instead of the
// shared gateway.
//
// A dispatched unit carries the LANE the scheduler picked, a board/workdir
// REFERENCE the agent resolves to its own checkout, and a correlation id. It
// never carries a task id: the scheduler picks the LANE, the foreman picks the
// TASK. So the only thing the scheduler has to know per lane is WHERE that
// lane's work should be handed — and that is a LANE PROPERTY, not a router
// decision (per-project pins in fleet.toml and the DB outrank the router, so a
// target chosen by the router could contradict them).
//
// STORAGE. Targets live in a JSONL file — the fleet's config shape for
// per-lane lists (groups.jsonl, templates.jsonl), append-only, diffable and
// readable by an operator without a migration:
//
//	{"lane":"helix","agent":"helix","workdir":"/home/bunker-helix/helix","board":"...","enabled":true}
//
// Path: $SCHEDULER_DISPATCH_TARGETS, else <home>/.hermes/coding-hermes/
// dispatch-targets.jsonl. A MISSING file is not an error and means NO lane is
// remote — a fleet that configures nothing behaves exactly as before, which is
// what makes this change additive.
//
// A lane absent from the file keeps its historical behavior (local spawn
// through the gateway). A lane present with enabled=false is explicitly local
// (an operator can park a target without deleting it). A lane present with an
// empty agent is refused on read and reported: a target that cannot be
// addressed must never look like a target that was handed out.
//
// NOTHING here falls back. There is no "if the agent is unreachable, use the
// gateway" branch anywhere in this feature: the dispatch leg either hands the
// work to the named agent or fails the tick loudly (dispatch-spec.md §2).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// EnvDispatchTargets names the file that declares which lanes are executed by
// a named agent. Unset/empty means the default path below.
const EnvDispatchTargets = "SCHEDULER_DISPATCH_TARGETS"

// DispatchTarget is one lane's execution target: the agent the work is handed
// to, plus the references the agent resolves on its own side.
type DispatchTarget struct {
	// Lane is the alias of the row's lane. The map key is authoritative (a
	// row whose lane field disagrees with its position in the file is
	// reported and skipped), so a copy-paste row cannot silently re-target
	// another lane.
	Lane string `json:"lane"`
	// Agent is the bus identity the work is handed to. Required.
	Agent string `json:"agent"`
	// Board is the board reference the agent resolves (path, or path@host for
	// a remote box). Empty = derive from the lane's workdir (the fleet's
	// .coding-hermes/board/tasks.jsonl convention).
	Board string `json:"board"`
	// Workdir is the checkout reference the agent resolves. Empty = the lane's
	// configured workdir (the control box's value).
	Workdir string `json:"workdir"`
	// HostID is the stable bunker identity used for cross-namespace capacity.
	// It is required for an enabled remote lane when usage-pool enforcement is on.
	HostID string `json:"host_id"`
	// Enabled defaults to true when absent (a row exists to say "this lane is
	// remote"); an explicit false parks the target without deleting it.
	Enabled *bool `json:"enabled"`
}

// IsEnabled reads the tri-state enabled flag (absent = enabled).
func (t DispatchTarget) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// DefaultDispatchTargetsPath resolves the target file: the env override, else
// <home>/.hermes/coding-hermes/dispatch-targets.jsonl. An unknown home yields
// "" (no file, no remote lanes — the additive default).
func DefaultDispatchTargetsPath() string {
	if p := strings.TrimSpace(os.Getenv(EnvDispatchTargets)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".hermes", "coding-hermes", "dispatch-targets.jsonl")
}

// dispatchTargetsMu guards the mtime cache so a tick storm does not re-read the
// file once per tick while it is unchanged. The file is operator config, read
// on the dispatch path only.
var (
	dispatchTargetsMu    sync.Mutex
	dispatchTargetsCache = map[string]dispatchTargetsEntry{}
)

type dispatchTargetsEntry struct {
	modUnixNano int64
	size        int64
	targets     map[string]DispatchTarget
}

// LoadDispatchTargets reads the lane→target map from a JSONL file. A missing
// file is (empty map, nil): "no remote lanes", the additive default. Malformed
// lines are REPORTED through the returned error alongside the rows that did
// parse, because silently dropping a target would let a lane the operator made
// remote quietly run locally — the exact silent divergence this feature exists
// to remove.
func LoadDispatchTargets(path string) (map[string]DispatchTarget, error) {
	out := map[string]DispatchTarget{}
	if strings.TrimSpace(path) == "" {
		return out, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, fmt.Errorf("open dispatch targets %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var problems []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		var t DispatchTarget
		if err := json.Unmarshal([]byte(raw), &t); err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		lane := strings.TrimSpace(t.Lane)
		if lane == "" {
			problems = append(problems, fmt.Sprintf("line %d: lane is required", line))
			continue
		}
		if prev, dup := out[lane]; dup && prev.Agent != t.Agent {
			problems = append(problems, fmt.Sprintf("line %d: lane %q already has agent %q (refusing to silently re-target)", line, lane, prev.Agent))
			continue
		}
		if t.IsEnabled() && strings.TrimSpace(t.Agent) == "" {
			problems = append(problems, fmt.Sprintf("line %d: lane %q is enabled with no agent — an unroutable target is refused", line, lane))
			continue
		}
		t.Lane = lane
		out[lane] = t
	}
	if err := sc.Err(); err != nil {
		problems = append(problems, fmt.Sprintf("read: %v", err))
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("dispatch targets %s: %s", path, strings.Join(problems, "; "))
	}
	return out, nil
}

// LookupDispatchTarget resolves one lane's execution target from the configured
// file, memoized on (mtime,size) so an unchanged file is not re-read per tick.
// The second return value is false when the lane is not remote — the common
// case, and byte-identical to the pre-1710 behavior.
//
// The memo keeps the LAST GOOD parse when a file edit breaks it: a broken
// config must not silently flip a remote lane back to local mid-flight. The
// parse error is returned so the caller can log it loudly.
func LookupDispatchTarget(path, lane string) (DispatchTarget, bool, error) {
	if strings.TrimSpace(lane) == "" {
		return DispatchTarget{}, false, nil
	}
	targets, err := cachedDispatchTargets(path)
	if err != nil {
		return DispatchTarget{}, false, err
	}
	t, ok := targets[lane]
	if !ok || !t.IsEnabled() {
		return DispatchTarget{}, false, nil
	}
	return t, true, nil
}

// cachedDispatchTargets returns the parsed file, re-reading it only when its
// size or mtime moved.
func cachedDispatchTargets(path string) (map[string]DispatchTarget, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]DispatchTarget{}, nil
	}
	st, statErr := os.Stat(path)

	dispatchTargetsMu.Lock()
	defer dispatchTargetsMu.Unlock()
	entry, cached := dispatchTargetsCache[path]
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// A deleted file is an operator statement: nothing is remote.
			dispatchTargetsCache[path] = dispatchTargetsEntry{targets: map[string]DispatchTarget{}}
			return map[string]DispatchTarget{}, nil
		}
		if cached {
			return entry.targets, nil
		}
		return map[string]DispatchTarget{}, fmt.Errorf("stat dispatch targets %s: %w", path, statErr)
	}
	if cached && entry.modUnixNano == st.ModTime().UnixNano() && entry.size == st.Size() {
		return entry.targets, nil
	}
	targets, err := LoadDispatchTargets(path)
	if err != nil && cached && len(entry.targets) > 0 {
		// Keep serving the last good parse; the caller still sees the error.
		return entry.targets, err
	}
	dispatchTargetsCache[path] = dispatchTargetsEntry{
		modUnixNano: st.ModTime().UnixNano(), size: st.Size(), targets: targets,
	}
	return targets, err
}

// resetDispatchTargetsCache drops the memo (tests, and the API's reload path).
func resetDispatchTargetsCache() {
	dispatchTargetsMu.Lock()
	defer dispatchTargetsMu.Unlock()
	dispatchTargetsCache = map[string]dispatchTargetsEntry{}
}

// resolveDispatchReferences fills the board/workdir references a dispatched
// unit carries. The references are what the AGENT resolves on its own side, so
// an explicit target value always wins; absent values fall back to the lane's
// configured workdir and the fleet's board convention inside it.
//
// The result is always non-empty for a lane with a workdir: a dispatch with no
// reference at all would hand an agent work it cannot locate.
func resolveDispatchReferences(target DispatchTarget, proj PackedProject) (board, workdir string) {
	workdir = strings.TrimSpace(target.Workdir)
	if workdir == "" {
		workdir = strings.TrimSpace(proj.Workdir)
	}
	board = strings.TrimSpace(target.Board)
	if board == "" && workdir != "" {
		board = filepath.Join(workdir, ".coding-hermes", "board", "tasks.jsonl")
	}
	return board, workdir
}
