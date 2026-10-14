package dashboard_test

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/dashboard"
	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1599 — live event toasts.
//
// The toast layer is three pieces that must stay in lockstep: the embedded
// client asset (static/js/events-toast.js), the shared layout's container +
// settings markup + CSS (templates/layout.html), and the URL path main.go
// mounts. These tests pin the load-bearing parts of each: a future edit that
// silently drops the rate cap, re-introduces a prompt-on-load notification
// request, un-suppresses routine churn, or points the <script src> at a path
// nothing serves must fail here instead of in a browser.

//go:embed static/js/events-toast.js
var toastFS embed.FS

func toastJS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(toastFS, "static/js/events-toast.js")
	if err != nil {
		t.Fatalf("read static/js/events-toast.js: %v", err)
	}
	return string(b)
}

// toastJSCode returns the asset with comment lines removed. Several properties
// below are asserted by occurrence count ("requestPermission appears exactly
// once"), and the file's own documentation names those APIs — counting them
// naively would fail on prose. Comments are not behaviour.
func toastJSCode(t *testing.T) string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(toastJS(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestSchedGap1599_ToastAssetEmbedded verifies the client is bundled into the
// binary (go:embed) and carries the pieces the acceptance criteria name.
func TestSchedGap1599_ToastAssetEmbedded(t *testing.T) {
	db := newTestDB(t)
	gen := dashboard.NewGenerator(db, nil)

	js := gen.EventsToastJS()
	if len(js) == 0 {
		t.Fatal("EventsToastJS returned empty bytes; the go:embed pattern likely missed the asset")
	}
	body := string(js)
	for _, want := range []string{
		"new window.EventSource(FEED_URL)", // SSE, not WebSocket
		"/api/v1/events/stream",
		"isSecureContext",   // insecure-origin explanation
		"requestPermission", // opt-in notification path
		"window.CHToast",    // the stats surface the acceptance evidence reads
		// Severity drives the rendered class, and CRITICAL (a real tier in the
		// events table) maps onto the HIGH notification tier instead of being
		// dropped by an INFO/MEDIUM/HIGH-only switch.
		"'toast toast-' + sev.toLowerCase()",
		"sev === 'CRITICAL'",
		"CRITICAL: 3",
		// A details-supplied lane must be shape-checked before it becomes a URL:
		// the api.auth refusal events carry target:"project X bump" (a
		// description), which used to resolve to /projects/project%20X%20bump.
		"if (lane && laneShaped(lane)) {",
		"/\\/projects\\/([A-Za-z0-9][A-Za-z0-9._-]*)/",
		"showAggregate",         // "+N more" summary path
		"resolveTarget",         // click-through resolution
		"/projects/", "/ticks/", // the two existing dashboard routes used
	} {
		if !strings.Contains(body, want) {
			t.Errorf("events-toast.js missing %q", want)
		}
	}
}

// TestSchedGap1599_ThrottleConstants pins the bounds (and the defaults) that
// make the toast count BOUNDED rather than 1:1 with the firehose.
func TestSchedGap1599_ThrottleConstants(t *testing.T) {
	js := toastJS(t)
	for _, want := range []string{
		"var DEDUPE_WINDOW_MS = 60000;",
		"var RATE_WINDOW_MS = 10000;",
		"var RATE_MAX = 5;",
		"var MAX_VISIBLE = 5;",
		"var AGGREGATE_AFTER = 5;",
		"var DEFAULT_THRESHOLD = 'MEDIUM';",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("throttle constant missing or changed without updating the test: %q", want)
		}
	}
	// The header must state the resulting bound so a reader does not have to
	// derive it from six constants.
	if !strings.Contains(js, "at most 5 toasts per rolling 10 seconds") {
		t.Error("the throttle design comment no longer states the aggregate bound")
	}
}

// TestSchedGap1599_QuietByDefault pins the two rules that keep a normal window
// silent: routine churn is dropped before the threshold test, and the default
// threshold is MEDIUM (not INFO).
func TestSchedGap1599_QuietByDefault(t *testing.T) {
	js := toastJS(t)
	code := toastJSCode(t)
	if !strings.Contains(js, `/^evaluation started\b/i`) || !strings.Contains(js, `/^evaluation (completed|finished)\b/i`) {
		t.Error("the routine-churn (quiet) patterns are gone — evaluation noise would toast")
	}
	if !strings.Contains(code, "var DEFAULT_THRESHOLD = 'MEDIUM';") {
		t.Error("the default threshold is no longer MEDIUM — quiet-by-default regressed")
	}
	// The routine test must run BEFORE the severity/threshold decision, or a
	// low threshold would re-admit evaluation churn.
	routine := strings.Index(code, "if (isRoutine(ev.message))")
	severity := strings.Index(code, "if (rank(sev) < rank(settings.threshold))")
	if routine < 0 || severity < 0 || routine > severity {
		t.Error("the routine-churn check must precede the threshold check in handleEvent")
	}
}

// TestSchedGap1599_NotificationOptInOnlyOnClick proves the permission request
// lives in exactly one place — the settings button's handler — so no code path
// (page load, SSE message, toast render) can prompt the user.
func TestSchedGap1599_NotificationOptInOnlyOnClick(t *testing.T) {
	js := toastJS(t)
	code := toastJSCode(t)
	if n := strings.Count(code, "requestPermission"); n != 1 {
		t.Fatalf("Notification.requestPermission appears %d times in CODE, want exactly 1 (inside enableNotifications)", n)
	}
	fn := strings.Index(code, "function enableNotifications()")
	call := strings.Index(code, "Notification.requestPermission()")
	if fn < 0 || call < 0 || call < fn {
		t.Error("the requestPermission call is not inside enableNotifications()")
	}
	// ... and enableNotifications is wired only as a click listener.
	if !strings.Contains(code, "enable.addEventListener('click', enableNotifications)") {
		t.Error("enableNotifications is not bound to the settings button's click event")
	}
	// The SSE client must be established without touching notification
	// permission: the permission call has to live textually before connect()'s
	// body, i.e. it can only be reached from the click wiring above it.
	if strings.Contains(code, "connect() {") && call > strings.Index(code, "function connect()") {
		t.Error("a requestPermission call appears inside/after the SSE client — prompting must not be a load-time path")
	}
	if !strings.Contains(js, "until the operator has enabled notifications in this UI") {
		t.Error("the double opt-in (permission AND an explicit switch) is no longer documented")
	}
}

// TestSchedGap1599_PageRendersToastLayer verifies every dashboard page carries
// the container, the settings control and the client <script> (the layer lives
// in the shared layout, so a page that opts out of the layout would silently
// lose it).
func TestSchedGap1599_PageRendersToastLayer(t *testing.T) {
	db := newTestDB(t)
	mustCreateProject(t, db, "alpha", 10, 5)
	gen := dashboard.NewGenerator(db, nil)

	pages := []struct {
		name   string
		render func(w *strings.Builder) error
	}{
		{"overview", func(w *strings.Builder) error { return gen.Generate(w) }},
		{"queue", func(w *strings.Builder) error { return gen.GenerateQueue(w) }},
		{"health", func(w *strings.Builder) error { return gen.GenerateHealth(w) }},
		{"tick history", func(w *strings.Builder) error { return gen.GenerateTickHistory(w, 1, database.TickFilter{}) }},
		{"project detail", func(w *strings.Builder) error { return gen.GenerateProjectDetail(w, "alpha") }},
	}
	for _, page := range pages {
		var buf strings.Builder
		if err := page.render(&buf); err != nil {
			t.Fatalf("render %s: %v", page.name, err)
		}
		out := buf.String()
		for _, want := range []string{
			`id="chToastStack"`,
			`aria-live="polite"`,
			`id="chNotifEnable"`,
			`id="chToastSev"`,
			`src="` + dashboard.EventsToastAssetPath + `"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s page missing %q", page.name, want)
			}
		}
	}
}

// TestSchedGap1599_AssetPathParity pins the template's <script src> to the path
// main.go mounts (dashboard.EventsToastAssetPath). Without this the page could
// reference a URL nothing serves and every acceptance check in a browser would
// fail with a silent 404.
func TestSchedGap1599_AssetPathParity(t *testing.T) {
	layout := layoutCSS(t)
	if !strings.Contains(layout, `src="`+dashboard.EventsToastAssetPath+`"`) {
		t.Fatalf("layout.html does not reference %q", dashboard.EventsToastAssetPath)
	}
	if dashboard.EventsToastAssetPath != "/static/js/events-toast.js" {
		t.Fatalf("asset path changed to %q — update the embed pattern and the route together", dashboard.EventsToastAssetPath)
	}
}

// TestSchedGap1599_RouteAndDocParity pins the other two ends of the same path:
// the route registration in cmd/schedulerd/main.go and its row in
// docs/reference/endpoints.md. The route MUST be a literal pattern — the
// internal/mcp endpoint-table parity guard (SCHED-GAP-166) scrapes route
// patterns out of that source, and a concatenated pattern reads as a route
// with an empty path, which that guard reports as drift.
func TestSchedGap1599_RouteAndDocParity(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	mainSrc, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "schedulerd", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/schedulerd/main.go: %v", err)
	}
	wantRoute := `mux.HandleFunc("GET ` + dashboard.EventsToastAssetPath + `"`
	if !strings.Contains(string(mainSrc), wantRoute) {
		t.Errorf("cmd/schedulerd/main.go does not register the asset as a literal pattern (%s) — the endpoint-table parity guard cannot see a concatenated one", wantRoute)
	}
	docs, err := os.ReadFile(filepath.Join(repoRoot, "docs", "reference", "endpoints.md"))
	if err != nil {
		t.Fatalf("read docs/reference/endpoints.md: %v", err)
	}
	if !strings.Contains(string(docs), "| `"+dashboard.EventsToastAssetPath+"` |") {
		t.Errorf("docs/reference/endpoints.md has no route-table row for %s (the endpoint-table parity guard requires one per live registration)", dashboard.EventsToastAssetPath)
	}
}

// TestSchedGap1599_SeverityTones pins the severity styling the criteria name:
// INFO neutral/blue, MEDIUM amber, HIGH/CRITICAL red — and that the INFO tone is
// actually defined (an undefined var() would render every INFO toast colourless).
func TestSchedGap1599_SeverityTones(t *testing.T) {
	layout := layoutCSS(t)
	for _, want := range []string{
		"--info:#6aa6f5",
		".toast-info{border-left-color:var(--info)}",
		".toast-medium{border-left-color:var(--warn)}",
		".toast-high{border-left-color:var(--err)}",
		".toast-critical{border-left-color:var(--err)}",
		".toast-aggregate{border-left-color:var(--muted)}",
		".toast-stack{position:fixed",
		"pointer-events:none", // the stack must not block the page
	} {
		if !strings.Contains(layout, want) {
			t.Errorf("layout.html missing severity/stack style %q", want)
		}
	}
}

// TestSchedGap1599_InsecureContextExplained pins the insecure-origin branch:
// the settings UI must explain WHY notifications are unavailable rather than
// rendering a button that can never work.
func TestSchedGap1599_InsecureContextExplained(t *testing.T) {
	js := toastJS(t)
	if !strings.Contains(js, "SECURE CONTEXT") || !strings.Contains(js, "isSecureContext = false") {
		t.Error("the insecure-context explanation text is gone — the panel would show a dead prompt")
	}
	if !strings.Contains(js, "button.hidden = (st === 'insecure' || st === 'unsupported')") {
		t.Error("the enable button is no longer hidden on an insecure/unsupported origin")
	}
	if !strings.Contains(js, "if (st === 'insecure' || st === 'unsupported') { refreshNotifUI(); return; }") {
		t.Error("enableNotifications no longer refuses to prompt on an insecure origin")
	}
}
