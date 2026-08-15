package dashboard

import (
	"context"
	"sort"
	"strings"
	"time"
)

// TaskType buckets a board task (or a completed tick's work) into a coarse
// work category so ETA/cost can learn per-type estimates instead of assuming
// every step is identical. The key insight: "2 steps left" is a bad signal
// when both are heavy implementation tasks, and "10 steps left" is a bad
// signal when all ten are fast smoke tests.
type TaskType string

const (
	TaskSpec  TaskType = "spec"
	TaskCode  TaskType = "code"
	TaskTest  TaskType = "test"
	TaskDocs  TaskType = "docs"
	TaskChore TaskType = "chore"
	TaskOther TaskType = "other"
)

// typeKeywords maps each TaskType to the substrings that route a title to it.
// Keywords are checked by scoring, not first-match, so "Tests: assert every
// kill-shot" routes to test even though it also mentions code.
var typeKeywords = map[TaskType][]string{
	TaskSpec:  {"spec", "schema", "plan", "design", "architect", "adr", "requirement", "10-section", "requirements"},
	TaskTest:  {"test", "assert", "kill-shot", "integration", "e2e", "unit", "coverage", "verify", "smoke", "scenario"},
	TaskDocs:  {"doc", "readme", "guide", "runbook"},
	TaskChore: {"chore", "bootstrap", "config", "ci", "dependenc", "toolchain", "refactor", "lint", "cleanup", "migrat"},
	TaskCode:  {"implement", "daemon", "shim", "engine", "core", "feat", "add ", "wire", "worker", "adapter", "provider", "workflow", "controller", "api", "policy", "audit", "protocol", "skeleton", "exec", "socket", "plugin", "path", "hash", "env", "relay", "resolve", "canonical", "parse", "fingerprint", "atomic", "idempot", "queue", "health"},
}

// priority orders tie-breaks. spec and test are checked with higher weight
// than code because a task can legitimately mention both (e.g. a test task
// named "tests for the daemon"). other is last.
var typePriority = []TaskType{TaskSpec, TaskTest, TaskDocs, TaskChore, TaskCode, TaskOther}

// classifyTaskType routes a task title or commit-work string to a TaskType by
// keyword score. Empty/unknown input falls back to TaskOther.
func classifyTaskType(title string) TaskType {
	low := strings.ToLower(title)
	scores := map[TaskType]int{}
	for typ, kws := range typeKeywords {
		for _, kw := range kws {
			if strings.Contains(low, kw) {
				scores[typ]++
			}
		}
	}
	best := TaskOther
	bestScore := 0
	for _, typ := range typePriority {
		s := scores[typ]
		if s > bestScore {
			best = typ
			bestScore = s
		}
	}
	return best
}

// typeSamples accumulates completed-tick duration AND cost for one TaskType.
// avg/avgCost are 0 until finalize() is called.
type typeSamples struct {
	count     int
	total     time.Duration
	avg       time.Duration
	totalCost float64
	avgCost   float64
}

func (s *typeSamples) add(d time.Duration, cost float64) {
	s.count++
	s.total += d
	s.totalCost += cost
}

func (s *typeSamples) finalize() {
	if s.count > 0 {
		s.avg = s.total / time.Duration(s.count)
		s.avgCost = s.totalCost / float64(s.count)
	}
}

// tickSample is one completed tick's duration + cost + the work it did (commit
// subject text), used to learn per-type estimates.
type tickSample struct {
	dur  time.Duration
	cost float64
	work string
}

// learnTypeSamples buckets completed-tick samples by classified type and
// computes the per-type average duration + cost. Returns nil-safe map.
func learnTypeSamples(samples []tickSample) map[TaskType]*typeSamples {
	learned := map[TaskType]*typeSamples{}
	for _, s := range samples {
		typ := classifyTaskType(s.work)
		ds, ok := learned[typ]
		if !ok {
			ds = &typeSamples{}
			learned[typ] = ds
		}
		ds.add(s.dur, s.cost)
	}
	for _, ds := range learned {
		ds.finalize()
	}
	return learned
}

// fleetModel is the fleet-wide learned prior: per-task-type average duration +
// cost aggregated across ALL projects, plus the fleet overall average. A new
// project starts from this prior and blends toward its own data as it grows.
type fleetModel struct {
	byType  map[TaskType]*typeSamples
	overall *typeSamples // fleet-wide average across all types
}

// fleetLearned aggregates completed-tick duration + cost across every project,
// bucketed by task type, to form the fleet-wide prior. Returns an empty model
// on query error so callers degrade to project-only estimates.
func (g *Generator) fleetLearned(ctx context.Context) *fleetModel {
	m := &fleetModel{byType: map[TaskType]*typeSamples{}, overall: &typeSamples{}}

	// Map project → workdir once, so we can classify each tick's commit work.
	wd := map[string]string{}
	if rows, err := g.db.QueryContext(ctx, `SELECT name, COALESCE(workdir,'') FROM projects`); err == nil {
		for rows.Next() {
			var name, w string
			if rows.Scan(&name, &w) == nil {
				wd[name] = w
			}
		}
		_ = rows.Close()
	}

	rows, err := g.db.QueryContext(ctx, `
		SELECT project_name, spawned_at, completed_at, cost_usd FROM ticks
		WHERE status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 200
	`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var proj, sp, co string
		var cost float64
		if rows.Scan(&proj, &sp, &co, &cost) != nil {
			continue
		}
		d := parseDuration(sp, co)
		if d <= 0 {
			continue
		}
		typ := classifyTaskType(tickWork(wd[proj], sp, co, 4))
		ds := m.byType[typ]
		if ds == nil {
			ds = &typeSamples{}
			m.byType[typ] = ds
		}
		ds.add(d, cost)
		m.overall.add(d, cost)
	}
	for _, ds := range m.byType {
		ds.finalize()
	}
	m.overall.finalize()
	return m
}

// projectBlendWeight returns the fraction of the blended estimate that comes
// from the current project (the rest comes from the fleet). It is a Bayesian-
// style sample-share weight with a small additive bias toward the project so a
// heavy/light project's character always registers — but the fleet keeps a
// majority because across many projects it is statistically more reliable than
// a project with only a few samples.
func projectBlendWeight(projectCount, fleetCount int) float64 {
	if fleetCount <= 0 {
		return 1.0 // no fleet signal → project only
	}
	if projectCount <= 0 {
		return 0.0 // no project signal → fleet only
	}
	const projectBias = 0.15 // slight bias toward the current project
	share := float64(projectCount) / float64(projectCount+fleetCount)
	w := share + projectBias*(1-share)
	if w > 0.5 {
		w = 0.5 // never let the project outvote the fleet prior
	}
	return w
}

// blendEstimate weights a project estimate against a fleet estimate by sample
// count, with the slight project bias from projectBlendWeight.
func blendEstimate(proj, fleet time.Duration, projCount, fleetCount int) time.Duration {
	w := projectBlendWeight(projCount, fleetCount)
	return time.Duration(float64(proj)*w + float64(fleet)*(1-w))
}

func blendCost(proj, fleet float64, projCount, fleetCount int) float64 {
	w := projectBlendWeight(projCount, fleetCount)
	return proj*w + fleet*(1-w)
}

// minLearnedSamples is the minimum per-type completed-tick count for a type to
// be considered "learned" (specific) rather than falling back to the overall
// average for that scope.
const minLearnedSamples = 2

// typeEstimate returns the blended duration + cost estimate for a pending task
// of the given type. It blends the project's per-type estimate (or project
// overall) against the fleet's per-type estimate (or fleet overall), weighted
// slightly toward the project. Returns a conservative floor when there is no
// signal at all.
func typeEstimate(typ TaskType, learned map[TaskType]*typeSamples, projectAvg time.Duration, projectAvgCost float64, projCount int, fleet *fleetModel, minSamples int) (time.Duration, float64) {
	// Project side: per-type if we have enough samples, else project overall.
	projDur, projCost, projN := projectAvg, projectAvgCost, projCount
	if ds, ok := learned[typ]; ok && ds.count > 0 {
		// Prefer the specific type average once learned; fall back to project
		// overall only when this type has no samples at all.
		projDur, projCost = ds.avg, ds.avgCost
		projN = ds.count
	}

	// Fleet side: per-type if learned, else fleet overall.
	fleetDur, fleetCost := time.Duration(0), float64(0)
	fleetN := 0
	if fleet != nil {
		if fds, ok := fleet.byType[typ]; ok && fds.count >= minSamples {
			fleetDur, fleetCost = fds.avg, fds.avgCost
			fleetN = fds.count
		} else if fleet.overall != nil && fleet.overall.count >= minSamples {
			fleetDur, fleetCost = fleet.overall.avg, fleet.overall.avgCost
			fleetN = fleet.overall.count
		}
	}

	if fleetN == 0 {
		if projDur > 0 {
			return projDur, projCost
		}
		return 15 * time.Minute, 0 // conservative floor when there is no signal
	}
	if projN == 0 || projDur <= 0 {
		return fleetDur, fleetCost // no project signal → pure fleet
	}
	return blendEstimate(projDur, fleetDur, projN, fleetN), blendCost(projCost, fleetCost, projN, fleetN)
}

// predictETA returns the remaining-time AND remaining-cost estimate for the
// pending board steps, plus per-type breakdowns and pending-step counts. It
// blends per-type project-learned estimates with the fleet-wide prior for both
// duration and cost. Done steps are ignored.
func predictETA(pending []BoardStep, learned map[TaskType]*typeSamples, projectAvg time.Duration, projectAvgCost float64, projCount int, fleet *fleetModel, minSamples int) (total time.Duration, totalCost float64, byType map[TaskType]time.Duration, counts map[TaskType]int) {
	byType = map[TaskType]time.Duration{}
	counts = map[TaskType]int{}
	for _, st := range pending {
		typ := classifyTaskType(st.Title)
		d, c := typeEstimate(typ, learned, projectAvg, projectAvgCost, projCount, fleet, minSamples)
		total += d
		totalCost += c
		byType[typ] += d
		counts[typ]++
	}
	return total, totalCost, byType, counts
}

// etaBreakdown renders a compact human-readable per-type breakdown for the ETA
// tooltip, e.g. "code 2·40m + test 5·5m". Ordered by contribution desc.
func etaBreakdown(byType map[TaskType]time.Duration, counts map[TaskType]int) string {
	type part struct {
		typ TaskType
		d   time.Duration
	}
	var parts []part
	for typ, d := range byType {
		if d > 0 {
			parts = append(parts, part{typ, d})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].d > parts[j].d })
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(" + ")
		}
		b.WriteString(string(p.typ))
		if n := counts[p.typ]; n > 1 {
			b.WriteString(" ×")
			b.WriteString(itoa(n))
		}
		b.WriteString(" ")
		b.WriteString(shortDur(p.d))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func shortDur(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return itoa(m) + "m"
	}
	h := m / 60
	r := m % 60
	if r == 0 {
		return itoa(h) + "h"
	}
	return itoa(h) + "h" + itoa(r) + "m"
}

// learnedETA computes the remaining-time AND remaining-cost estimate for a
// project from its board and tick history, blending per-task-type estimates
// with the fleet-wide prior (project-slightly-biased weighted blend) for both
// duration and cost.
//
// Returns (eta, completionAtRFC3339, breakdown, projectedCost). eta is 0 when
// there is no signal (no board or no history) so callers fall back to the old
// naive math.
func (g *Generator) learnedETA(ctx context.Context, project, workdir string, steps []BoardStep, fleet *fleetModel) (time.Duration, string, string, float64) {
	if project == "" || len(steps) == 0 {
		return 0, "", "", 0
	}

	// Gather recent completed ticks + their work + cost, for per-type learning.
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at, cost_usd FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 20
	`, project)
	var samples []tickSample
	var projTotal time.Duration
	var projTotalCost float64
	var projCount int
	if err == nil {
		for rows.Next() {
			var sp, co string
			var cost float64
			if rows.Scan(&sp, &co, &cost) == nil {
				if d := parseDuration(sp, co); d > 0 {
					samples = append(samples, tickSample{dur: d, cost: cost, work: tickWork(workdir, sp, co, 4)})
					projTotal += d
					projTotalCost += cost
					projCount++
				}
			}
		}
		_ = rows.Close()
	}

	learned := learnTypeSamples(samples)
	var projectAvg time.Duration
	var projectAvgCost float64
	if projCount > 0 {
		projectAvg = projTotal / time.Duration(projCount)
		projectAvgCost = projTotalCost / float64(projCount)
	}

	// Pending steps are anything not done (active + pending).
	var pending []BoardStep
	for _, s := range steps {
		if s.Status != "done" {
			pending = append(pending, s)
		}
	}
	if len(pending) == 0 {
		return 0, "", "", 0
	}

	total, totalCost, byType, counts := predictETA(pending, learned, projectAvg, projectAvgCost, projCount, fleet, minLearnedSamples)
	if total <= 0 {
		return 0, "", "", 0
	}
	completionAt := time.Now().UTC().Add(total).Format(time.RFC3339)
	return total, completionAt, etaBreakdown(byType, counts), totalCost
}
