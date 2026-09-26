package scheduler

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

func cadenceFloat(v float64) *float64 { return &v }

func cadenceProject(name string, target *float64) database.Project {
	ns := "cadence"
	pin := 21600
	return database.Project{
		Name: name, RepoURL: "https://example.com/" + name, Workdir: "/tmp/" + name,
		Weight: 1, Priority: 5, CooldownS: 21600, CooldownPinS: &pin,
		DecayRate: 1, Enabled: true, NamespaceID: &ns,
		CreatedAt:        time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		TargetRunsPerDay: target,
	}
}

func TestCadenceTarget_BelowTargetOutranksLaneMeetingTarget(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	projects := []database.Project{
		cadenceProject("a-meeting", cadenceFloat(4)),
		cadenceProject("z-below", cadenceFloat(4)),
	}
	ns := []database.Namespace{{ID: "cadence", Weight: 100, Reserved: 100, HardCap: 100, Enabled: true}}
	last := map[string]time.Time{
		"a-meeting": now.Add(-7 * time.Hour),
		"z-below":   now.Add(-7 * time.Hour),
	}
	p := NewMultiPoolPacker(100, 1, nil)
	p.SetCadenceRates(map[string]float64{"a-meeting": 4, "z-below": 1})
	got := p.Pack(projects, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), last, nil, now)
	if len(got.Projects) != 1 || got.Projects[0].Name != "z-below" {
		t.Fatalf("selected = %#v, want only z-below (below target outranks meeting target)", got.Projects)
	}
}

func TestCadenceTarget_AbsentTargetPreservesCurrentOrdering(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	// No override and no cooldown pin means no cadence opinion. A cadence-rate
	// snapshot must therefore produce the same winner as the pre-feature path.
	a := cadenceProject("a-current-first", nil)
	z := cadenceProject("z-current-second", nil)
	a.CooldownPinS = nil
	z.CooldownPinS = nil
	projects := []database.Project{z, a}
	ns := []database.Namespace{{ID: "cadence", Weight: 100, Reserved: 100, HardCap: 100, Enabled: true}}
	last := map[string]time.Time{
		a.Name: now.Add(-7 * time.Hour),
		z.Name: now.Add(-7 * time.Hour),
	}
	pack := func(rates map[string]float64) []PackedProject {
		p := NewMultiPoolPacker(100, 1, nil)
		p.SetCadenceRates(rates)
		return p.Pack(projects, ns, NewUrgencyCalculator(time.Minute, time.Hour, 10), last, nil, now).Projects
	}
	baseline := pack(nil)
	withRates := pack(map[string]float64{a.Name: 0, z.Name: 999})
	if len(baseline) != 1 || len(withRates) != 1 || baseline[0].Name != withRates[0].Name {
		t.Fatalf("absent targets changed ordering: baseline=%#v withRates=%#v", baseline, withRates)
	}
}

func TestEffectiveCadenceTarget_DerivedThenOverrideThenOptOut(t *testing.T) {
	p := cadenceProject("lane", nil)
	if got, ok := effectiveCadenceTarget(p); !ok || got != 4 {
		t.Fatalf("derived target = (%v,%v), want (4,true)", got, ok)
	}
	p.TargetRunsPerDay = cadenceFloat(3)
	if got, ok := effectiveCadenceTarget(p); !ok || got != 3 {
		t.Fatalf("override target = (%v,%v), want (3,true)", got, ok)
	}
	p.TargetRunsPerDay = cadenceFloat(0)
	if _, ok := effectiveCadenceTarget(p); ok {
		t.Fatal("explicit zero target must mean no cadence opinion")
	}
}
