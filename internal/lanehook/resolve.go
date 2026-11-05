package lanehook

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// rank orders the three levels: global before namespace before lane.
func rank(l Level) int {
	switch l {
	case LevelGlobal:
		return 0
	case LevelNamespace:
		return 1
	case LevelLane:
		return 2
	}
	return 3
}

// Validate checks a whole set before it is armed. Every problem is reported
// with the offending id so the API can return one actionable message.
func Validate(set []Step) error {
	seen := make(map[string]int, len(set))
	for i, s := range set {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("hook #%d: id is required", i+1)
		}
		if s.Phase != PhasePre && s.Phase != PhasePost {
			return fmt.Errorf("hook %q: phase must be pre or post, got %q", s.ID, s.Phase)
		}
		if s.Level != LevelGlobal && s.Level != LevelNamespace && s.Level != LevelLane {
			return fmt.Errorf("hook %q: level must be global, namespace or lane, got %q", s.ID, s.Level)
		}
		if s.Target == "" && s.Level == LevelGlobal {
			// global steps conventionally target "*"; normalise below in the loader.
			return fmt.Errorf("hook %q: target is required (use \"*\" for global)", s.ID)
		}
		switch s.OnFailureOrDefault() {
		case FailClosed, FailOpen, FailRetry:
		default:
			return fmt.Errorf("hook %q: on_failure must be closed, open or retry, got %q", s.ID, s.OnFailure)
		}
		if !filepath.IsAbs(s.Command) {
			return fmt.Errorf("hook %q: command must be an absolute path, got %q", s.ID, s.Command)
		}
		if s.TimeoutS < 0 {
			return fmt.Errorf("hook %q: timeout_s must be >= 0", s.ID)
		}
		key := string(s.Phase) + "|" + string(s.Level) + "|" + s.Target + "|" + s.ID
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("hook %q: duplicate (phase=%s level=%s target=%s) — first declared at index %d",
				s.ID, s.Phase, s.Level, s.Target, prev)
		}
		seen[key] = i
	}
	return nil
}

// matches reports whether a step applies to this lane.
func matches(s Step, lane, namespace string) bool {
	switch s.Level {
	case LevelGlobal:
		return s.Target == "*" || s.Target == ""
	case LevelNamespace:
		return namespace != "" && s.Target == namespace
	case LevelLane:
		return s.Target == lane
	}
	return false
}

// Resolve builds the ordered plan for one lane (S13 §2.2). It is pure: no I/O,
// no clock, deterministic for a given input.
func Resolve(set []Step, lane, namespace, workdir, repo string, order Order, skip []string) (Plan, error) {
	if order == "" {
		order = OrderLevelFirst
	}
	if order != OrderLevelFirst && order != OrderFlat {
		return Plan{}, fmt.Errorf("hook_order must be %q or %q, got %q", OrderLevelFirst, OrderFlat, order)
	}
	skipSet := make(map[string]struct{}, len(skip))
	for _, id := range skip {
		skipSet[id] = struct{}{}
	}

	var pre, post []Step
	for _, s := range set {
		if !matches(s, lane, namespace) || !s.IsEnabled() {
			continue
		}
		if _, skipped := skipSet[s.ID]; skipped {
			continue
		}
		if s.Phase == PhasePre {
			pre = append(pre, s)
		} else {
			post = append(post, s)
		}
	}

	if order == OrderFlat {
		sortSteps(pre, true, true)    // priority asc, level asc
		sortSteps(post, false, false) // priority desc, level desc
	} else {
		pre = levelOrdered(pre, true)
		post = levelOrdered(post, false)
	}

	return Plan{
		Lane:      lane,
		Namespace: namespace,
		Workdir:   workdir,
		Repo:      repo,
		Order:     order,
		Pre:       pre,
		Post:      post,
	}, nil
}

// levelOrdered groups by level then sorts each group (S13 §2.2).
func levelOrdered(in []Step, ascending bool) []Step {
	groups := [3][]Step{}
	for _, s := range in {
		groups[rank(s.Level)] = append(groups[rank(s.Level)], s)
	}
	out := make([]Step, 0, len(in))
	for _, g := range groups {
		sortSteps(g, ascending, true)
		out = append(out, g...)
	}
	if !ascending { // POST runs lane -> namespace -> global
		rev := make([]Step, 0, len(out))
		for i := range groups {
			g := groups[len(groups)-1-i]
			rev = append(rev, g...)
		}
		out = rev
	}
	return out
}

// sortSteps orders by (priority, level, id). ascending=false reverses priority
// and level but keeps id ascending so ties stay deterministic.
func sortSteps(in []Step, ascending, levelAscending bool) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Priority != b.Priority {
			if ascending {
				return a.Priority < b.Priority
			}
			return a.Priority > b.Priority
		}
		if rank(a.Level) != rank(b.Level) {
			if levelAscending {
				return rank(a.Level) < rank(b.Level)
			}
			return rank(a.Level) > rank(b.Level)
		}
		return a.ID < b.ID
	})
}

// Expand substitutes the placeholder set into a command's arguments. It never
// introduces a shell: the caller passes the result straight to exec.
func Expand(args []string, env Env) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		r := strings.NewReplacer(
			"{lane}", env.Lane,
			"{project}", env.Project,
			"{role}", env.Role,
			"{namespace}", env.Namespace,
			"{workdir}", env.Workdir,
			"{repo}", env.Repo,
			"{tick}", env.TickID,
			"{phase}", string(env.Phase),
			"{hook}", env.HookID,
		)
		out[i] = r.Replace(a)
	}
	return out
}
