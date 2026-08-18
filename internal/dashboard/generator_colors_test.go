package dashboard_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// TestGenerateQueue_NoZgotmplZ is the GAP-055 regression test. html/template's
// CSS sanitizer (cssValueFilter) replaces values it cannot parse inside inline
// style="..." attributes with the literal string "ZgotmplZ". urgencyColor used
// to return "var(--green)"/"var(--yellow)"/"var(--red)", so every urgency bar
// on the queue page rendered with an unparseable background (only the numeric
// % survived). The helper now returns literal hex colors from the layout
// palette; the rendered queue page must contain zero ZgotmplZ markers and one
// parseable hex background per urgency bar.
func TestGenerateQueue_NoZgotmplZ(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	// Urgency = priority * (1 + hours since last tick). Priority 10 with tick
	// ages 1h/10h/100h covers all three color branches: green (<50),
	// yellow (<200), red (>=200).
	for _, name := range []string{"alpha", "beta", "gamma"} {
		mustCreateProject(t, db, name, 10, 10)
	}
	mustCreateTick(t, db, "alpha-tick", "alpha", now.Add(-time.Hour))
	mustCreateTick(t, db, "beta-tick", "beta", now.Add(-10*time.Hour))
	mustCreateTick(t, db, "gamma-tick", "gamma", now.Add(-100*time.Hour))

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.GenerateQueue(&buf); err != nil {
		t.Fatalf("GenerateQueue: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "ZgotmplZ") {
		t.Fatalf("rendered queue page contains html/template sanitizer marker %q; urgency bar backgrounds are unparseable: %s", "ZgotmplZ", snippet(out, "urgency-bar"))
	}

	hexBg := regexp.MustCompile(`background:#[0-9a-fA-F]{6}`)
	if got := hexBg.FindAllString(out, -1); len(got) < 3 {
		t.Errorf("expected at least 3 urgency bars with hex backgrounds, got %d: %v", len(got), got)
	}
	for _, want := range []string{`background:#37d399`, `background:#e8a33d`, `background:#ff6b6b`} {
		if !strings.Contains(out, want) {
			t.Errorf("queue page missing %s (urgency green <50, yellow <200, red >=200)", want)
		}
	}
}

// TestGenerate_NoZgotmplZ_NamespaceUtilBars covers the fleet overview's
// namespace utilization bars, which use utilColor through the same inline
// style attribute. utilization = used / allocated * 100, so the seeded ticks
// exercise yellow (60%), red (110%), and green (no tick → 0%).
func TestGenerate_NoZgotmplZ_NamespaceUtilBars(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if err := database.CreateNamespace(ctx, db, &database.Namespace{ID: "warn-ns", Weight: 30, Reserved: 10, HardCap: 100, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace warn-ns: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{TickGroup: "gap-055", NamespaceID: "warn-ns", Allocated: 100, Used: 60}); err != nil {
		t.Fatalf("InsertNamespaceTick warn-ns: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{ID: "err-ns", Weight: 30, Reserved: 10, HardCap: 100, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace err-ns: %v", err)
	}
	if err := database.InsertNamespaceTick(ctx, db, &database.NamespaceTick{TickGroup: "gap-055", NamespaceID: "err-ns", Allocated: 100, Used: 110}); err != nil {
		t.Fatalf("InsertNamespaceTick err-ns: %v", err)
	}
	if err := database.CreateNamespace(ctx, db, &database.Namespace{ID: "ok-ns", Weight: 30, Reserved: 10, HardCap: 100, Enabled: true}); err != nil {
		t.Fatalf("CreateNamespace ok-ns: %v", err)
	}

	g := dashboard.NewGenerator(db)
	var buf strings.Builder
	if err := g.Generate(&buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "ZgotmplZ") {
		t.Fatalf("rendered fleet overview contains html/template sanitizer marker %q; utilization bar backgrounds are unparseable: %s", "ZgotmplZ", snippet(out, "Namespaces"))
	}
	for _, want := range []string{`background:#37d399`, `background:#e8a33d`, `background:#ff6b6b`} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet overview missing %s in namespace utilization bars (util green <50, yellow >=50, red >80)", want)
		}
	}
}
