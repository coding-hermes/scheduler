package lanehook

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
)

// LaneSpec is the per-lane worktree declaration (S14 §2.1).
type LaneSpec struct {
	Repo      string   `toml:"repo"      json:"repo"`
	Base      string   `toml:"base"      json:"base"`
	Policy    string   `toml:"policy"    json:"policy"`
	Push      bool     `toml:"push"      json:"push"`
	FloorGiB  float64  `toml:"floor_gib"  json:"floor_gib"`
	Gates     []string `toml:"gates"      json:"gates"`
	SkipHooks []string `toml:"skip_hooks" json:"skip_hooks"`
}

// SchedulerBlock is the [scheduler] table of hooks.toml.
type SchedulerBlock struct {
	HookOrder        string `toml:"hook_order"`
	HookTotalBudgetS int    `toml:"hook_total_budget_s"`
}

// File is the whole hooks.toml document.
type File struct {
	Scheduler SchedulerBlock      `toml:"scheduler"`
	Hooks     []Step              `toml:"hook"`
	Lanes     map[string]LaneSpec `toml:"lane"`
}

// LoadFile reads and validates hooks.toml. A missing file is not an error: it
// means "no hooks", which is the normal state of a fleet that has not adopted
// S13 yet (S13 §5).
func LoadFile(path string) (*File, error) {
	f := &File{Lanes: map[string]LaneSpec{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, err
	}
	if _, err := toml.Decode(string(data), f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Normalise: a global step may omit target.
	for i := range f.Hooks {
		if f.Hooks[i].Level == LevelGlobal && f.Hooks[i].Target == "" {
			f.Hooks[i].Target = "*"
		}
	}
	if err := Validate(f.Hooks); err != nil {
		return nil, err
	}
	if f.Scheduler.HookOrder != "" && f.Scheduler.HookOrder != string(OrderLevelFirst) && f.Scheduler.HookOrder != string(OrderFlat) {
		return nil, fmt.Errorf("[scheduler].hook_order must be level_first or flat, got %q", f.Scheduler.HookOrder)
	}
	return f, nil
}

// Order returns the configured cross-level ordering rule.
func (f *File) Order() Order {
	if f == nil || f.Scheduler.HookOrder == "" {
		return OrderLevelFirst
	}
	return Order(f.Scheduler.HookOrder)
}

// Set holds the armed hook definitions and reloads them when the file changes.
// A parse error keeps the PREVIOUS set armed (S13 §4.1) — a broken edit must
// never disarm a running fleet.
type Set struct {
	mu      sync.RWMutex
	path    string
	file    *File
	mtime   time.Time
	size    int64
	lastErr error
}

// Open loads path and returns a Set. A broken file yields an empty-but-armed
// set plus the error, so the daemon can log and continue.
func Open(path string) (*Set, error) {
	s := &Set{path: path}
	f, err := LoadFile(path)
	if err != nil {
		s.file = &File{Lanes: map[string]LaneSpec{}}
		s.lastErr = err
		return s, err
	}
	s.file = f
	s.stamp()
	return s, nil
}

func (s *Set) stamp() {
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime, s.size = fi.ModTime(), fi.Size()
	}
}

// Path returns the hooks file path this set watches.
func (s *Set) Path() string { return s.path }

// LastError returns the error from the most recent failed reload, or nil.
func (s *Set) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// ReloadIfChanged re-reads the file when its mtime or size moved. It returns
// (changed, err); on error the previous set stays armed.
func (s *Set) ReloadIfChanged() (bool, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.file != nil && len(s.file.Hooks) == 0 {
				return false, nil
			}
			s.file = &File{Lanes: map[string]LaneSpec{}}
			s.mtime, s.size = time.Time{}, 0
			return true, nil
		}
		return false, err
	}
	s.mu.RLock()
	unchanged := fi.ModTime().Equal(s.mtime) && fi.Size() == s.size
	s.mu.RUnlock()
	if unchanged {
		return false, nil
	}
	f, err := LoadFile(s.path)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return false, err
	}
	s.mu.Lock()
	s.file = f
	s.lastErr = nil
	s.mtime, s.size = fi.ModTime(), fi.Size()
	s.mu.Unlock()
	return true, nil
}

// Steps returns a copy of the armed definitions.
func (s *Set) Steps() []Step {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return nil
	}
	out := make([]Step, len(s.file.Hooks))
	copy(out, s.file.Hooks)
	return out
}

// Order returns the armed ordering rule.
func (s *Set) Order() Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.Order()
}

// Lane returns the worktree declaration for a lane.
func (s *Set) Lane(name string) (LaneSpec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return LaneSpec{}, false
	}
	l, ok := s.file.Lanes[name]
	return l, ok
}

// PlanFor resolves the plan for one lane from the armed set.
func (s *Set) PlanFor(lane, namespace, workdir, repo string, skip []string) (Plan, error) {
	s.mu.RLock()
	steps, order := s.file.Hooks, s.file.Order()
	s.mu.RUnlock()
	return Resolve(steps, lane, namespace, workdir, repo, order, skip)
}
