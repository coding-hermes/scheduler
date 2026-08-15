package dashboard

import (
	"testing"
	"time"
)

func TestClassifyTaskType(t *testing.T) {
	cases := []struct {
		title string
		want  TaskType
	}{
		{"T01 SPEC phase: write 10-section specs for daemon", TaskSpec},
		{"T02 Daemon skeleton: root systemd service, framed socket protocol", TaskCode},
		{"T03 Canonical-path + byte-hash resolution; clean env", TaskCode},
		{"T10 Tests: assert every kill-shot from red-team R1-R19", TaskTest},
		{"Integration scenario coverage", TaskTest},
		{"docs: add a deploy-an-application guide", TaskDocs},
		{"chore: bootstrap sudobroker repo (Go gates, hilo, board)", TaskChore},
		{"fix: ci toolchain pinning", TaskChore},
		{"random unclear title", TaskOther},
	}
	for _, c := range cases {
		if got := classifyTaskType(c.title); got != c.want {
			t.Errorf("classifyTaskType(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestLearnTypeSamplesAndPredict(t *testing.T) {
	// History: spec tasks ~30m/$0.03, test tasks ~5m/$0.01, code ~20m/$0.02.
	samples := []tickSample{
		{20 * time.Minute, 0.02, "feat(daemon): T02 daemon skeleton"},
		{30 * time.Minute, 0.03, "feat(specs): T01 10-section specs"},
		{29 * time.Minute, 0.03, "feat(specs): T05 requirements schema"},
		{5 * time.Minute, 0.01, "test: T10 kill-shot test suite"},
		{21 * time.Minute, 0.02, "feat(engine): T03 safe exec"},
		{6 * time.Minute, 0.01, "test: T10 e2e scenario"},
	}
	learned := learnTypeSamples(samples)

	pending := []BoardStep{
		{ID: "T05", Title: "Spec: requirements doc", Status: "pending"},
		{ID: "T06", Title: "Implement shim daemon core", Status: "pending"},
		{ID: "T07", Title: "Test scenario A", Status: "pending"},
		{ID: "T08", Title: "Test scenario B", Status: "pending"},
		{ID: "T09", Title: "Test scenario C", Status: "pending"},
		{ID: "T10", Title: "Test scenario D", Status: "pending"},
		{ID: "T11", Title: "Test scenario E", Status: "pending"},
	}

	// fleet nil → pure project. spec ~29.5m, code ~20.5m, test ~5.5m → ~78m.
	total, totalCost, byType, counts := predictETA(pending, learned, 0, 0, 0, nil, 2)
	if total < 60*time.Minute || total > 100*time.Minute {
		t.Errorf("predictETA total = %v, want ~78m", total)
	}
	if counts[TaskSpec] != 1 || counts[TaskCode] != 1 || counts[TaskTest] != 5 {
		t.Errorf("counts = %v, want spec=1 code=1 test=5", counts)
	}
	if byType[TaskSpec] < 25*time.Minute || byType[TaskSpec] > 34*time.Minute {
		t.Errorf("byType spec=%v, want ~29.5m", byType[TaskSpec])
	}
	// Cost: spec ~0.03 + code ~0.02 + 5×~0.01 ≈ 0.10.
	if totalCost < 0.08 || totalCost > 0.12 {
		t.Errorf("totalCost = %v, want ~0.10", totalCost)
	}
}

func TestWeightedBlendProjectBias(t *testing.T) {
	// Project has 2 samples of its type; fleet has 100. Blend should lean
	// toward the fleet (majority) but keep the project's influence present.
	projCount, fleetCount := 2, 100
	w := projectBlendWeight(projCount, fleetCount)
	if w > 0.5 {
		t.Errorf("project weight %v should never exceed 0.5", w)
	}
	if w < 0.1 {
		t.Errorf("project weight %v too small — project should have some influence", w)
	}
	// Blend: project=10m, fleet=20m → result between the two, closer to fleet.
	blended := blendEstimate(10*time.Minute, 20*time.Minute, projCount, fleetCount)
	if blended <= 10*time.Minute || blended >= 20*time.Minute {
		t.Errorf("blend = %v, want strictly between 10m and 20m", blended)
	}
	if blended < 18*time.Minute {
		t.Errorf("blend = %v, want closer to fleet (20m) than project (10m)", blended)
	}
}

func TestPredictBlendsProjectAndFleet(t *testing.T) {
	// Project's code estimate is 40m (a heavy project); fleet code avg is 20m.
	// The blend should be pulled toward the fleet but keep the project's
	// heaviness registered.
	learned := map[TaskType]*typeSamples{
		TaskCode: {count: 2, avg: 40 * time.Minute, avgCost: 0.04},
	}
	fleet := &fleetModel{
		byType: map[TaskType]*typeSamples{
			TaskCode: {count: 100, avg: 20 * time.Minute, avgCost: 0.02},
		},
		overall: &typeSamples{count: 200, avg: 15 * time.Minute, avgCost: 0.015},
	}
	pending := []BoardStep{{ID: "T01", Title: "Implement the daemon executor", Status: "pending"}}
	total, _, _, _ := predictETA(pending, learned, 0, 0, 2, fleet, 2)
	if total <= 20*time.Minute || total >= 40*time.Minute {
		t.Errorf("blended total = %v, want between fleet 20m and project 40m", total)
	}
	// Since fleet has many more samples, should be closer to 20m than 40m.
	if total > 30*time.Minute {
		t.Errorf("blended total = %v, want closer to fleet (20m)", total)
	}
}

func TestPredictColdStartUsesFleet(t *testing.T) {
	// New project, no samples at all → pure fleet prior, never the floor.
	learned := map[TaskType]*typeSamples{}
	fleet := &fleetModel{
		byType: map[TaskType]*typeSamples{
			TaskCode: {count: 100, avg: 22 * time.Minute, avgCost: 0.022},
		},
		overall: &typeSamples{count: 200, avg: 12 * time.Minute, avgCost: 0.012},
	}
	pending := []BoardStep{{ID: "T01", Title: "Implement the daemon executor", Status: "pending"}}
	total, cost, _, _ := predictETA(pending, learned, 0, 0, 0, fleet, 2)
	if total != 22*time.Minute {
		t.Errorf("cold-start total = %v, want fleet code avg 22m", total)
	}
	if cost < 0.02 || cost > 0.025 {
		t.Errorf("cold-start cost = %v, want ~0.022", cost)
	}

	// Unknown type not in fleet.byType → fleet overall.
	pending2 := []BoardStep{{ID: "T02", Title: "Something with no keywords at all", Status: "pending"}}
	total2, cost2, _, _ := predictETA(pending2, learned, 0, 0, 0, fleet, 2)
	if total2 != 12*time.Minute {
		t.Errorf("fleet-overall fallback total = %v, want 12m", total2)
	}
	if cost2 < 0.01 || cost2 > 0.014 {
		t.Errorf("fleet-overall fallback cost = %v, want ~0.012", cost2)
	}
}

func TestEtaBreakdown(t *testing.T) {
	byType := map[TaskType]time.Duration{
		TaskTest: 5 * time.Minute,
		TaskCode: 40 * time.Minute,
	}
	counts := map[TaskType]int{TaskTest: 5, TaskCode: 1}
	s := etaBreakdown(byType, counts)
	if s == "" {
		t.Error("etaBreakdown returned empty")
	}
	if got := shortDur(75 * time.Minute); got != "1h15m" {
		t.Errorf("shortDur(75m) = %q, want 1h15m", got)
	}
}
