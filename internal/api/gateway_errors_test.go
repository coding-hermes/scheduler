package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// Regression tests for SCHED-GAP-1654's API half: GET /api/v1/gateway-errors
// must report HOW MANY ticks were not run because of gateway unavailability,
// split by error class and lane, over a fixed 24h window.
//
// Two independent halves are pinned:
//
//   - the WIRE (seeded rows): window boundary, closed class vocabulary,
//     per-project grouping/order, empty-window shape, method gate;
//   - the PRODUCER (the end-to-end test at the bottom): a REAL Spawner driven
//     against a mock gateway that serves 503/429 writes the very rows this
//     endpoint counts — proving the endpoint reads the shape the spawn path
//     emits, not a shape invented for the test.

// gwErrProject mirrors one by_project entry.
type gwErrProject struct {
	Project string           `json:"project"`
	Total   int64            `json:"total"`
	ByClass map[string]int64 `json:"by_class"`
}

// gwErrResp mirrors the endpoint's response body.
type gwErrResp struct {
	GeneratedAt string           `json:"generated_at"`
	WindowHours int              `json:"window_hours"`
	Cutoff      string           `json:"cutoff"`
	Total       int64            `json:"total"`
	ByClass     map[string]int64 `json:"by_class"`
	ByProject   []gwErrProject   `json:"by_project"`
}

// schedGap1654Fetch GETs the gateway-errors surface and returns the status,
// the decoded body and the RAW body (the raw text is what proves the empty
// case serializes an array rather than null).
func schedGap1654Fetch(t *testing.T, baseURL string) (int, gwErrResp, string) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/v1/gateway-errors")
	if err != nil {
		t.Fatalf("GET /api/v1/gateway-errors: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out gwErrResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode body %s: %v", raw, err)
		}
	}
	return resp.StatusCode, out, string(raw)
}

// TestSCHEDGAP1654_GatewayErrorsEndpoint_WindowAndShape pins the wire contract
// with directly seeded rows: the 24h boundary excludes older rows, every class
// is present even at zero, projects are grouped and ordered by volume, and the
// per-project breakdown is closed too.
func TestSCHEDGAP1654_GatewayErrorsEndpoint_WindowAndShape(t *testing.T) {
	a := newAPITestServer(t)
	now := time.Now().UTC()

	seed := func(project, class string, at time.Time) {
		t.Helper()
		details := `{"event_type":"` + scheduler.GatewayAvailabilityEventType +
			`","error_class":"` + class + `","project":"` + project +
			`","tick_id":"t-` + class + `-` + project + `","error":"mock","timestamp":"` +
			at.Format(time.RFC3339) + `"}`
		if _, err := a.db.Exec(
			`INSERT INTO events (severity, component, message, details, created_at) VALUES (?,?,?,?,?)`,
			"HIGH", scheduler.GatewayAvailabilityEventComponent,
			"gateway unavailable: "+class, details, at.Format(time.RFC3339)); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	// Inside the window: two classes on lane-a (503 x2, 429 x1) and one on
	// lane-b (refused). Outside it: one more on lane-b, 25h old.
	seed("lane-a", scheduler.GatewayErrClassUnavailable503, now.Add(-1*time.Hour))
	seed("lane-a", scheduler.GatewayErrClassUnavailable503, now.Add(-2*time.Hour))
	seed("lane-a", scheduler.GatewayErrClassRateLimited429, now.Add(-3*time.Hour))
	seed("lane-b", scheduler.GatewayErrClassConnRefused, now.Add(-30*time.Minute))
	seed("lane-b", scheduler.GatewayErrClassUnavailable503, now.Add(-25*time.Hour))

	status, body, _ := schedGap1654Fetch(t, a.ts.URL)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body.WindowHours != 24 {
		t.Errorf("window_hours = %d, want 24", body.WindowHours)
	}
	if body.Cutoff == "" || body.GeneratedAt == "" {
		t.Errorf("cutoff/generated_at missing: cutoff=%q generated_at=%q", body.Cutoff, body.GeneratedAt)
	}
	if body.Total != 4 {
		t.Errorf("total = %d, want 4 (the 25h-old row must be outside the window)", body.Total)
	}
	// Closed vocabulary: every class present, including the class with no rows.
	for _, class := range []string{
		scheduler.GatewayErrClassUnavailable503,
		scheduler.GatewayErrClassRateLimited429,
		scheduler.GatewayErrClassConnRefused,
	} {
		if _, ok := body.ByClass[class]; !ok {
			t.Errorf("by_class is missing %q — a reader must read 0, not an absent field", class)
		}
	}
	if got := body.ByClass[scheduler.GatewayErrClassUnavailable503]; got != 2 {
		t.Errorf("by_class[unavailable_503] = %d, want 2", got)
	}
	if got := body.ByClass[scheduler.GatewayErrClassRateLimited429]; got != 1 {
		t.Errorf("by_class[rate_limited_429] = %d, want 1", got)
	}
	if got := body.ByClass[scheduler.GatewayErrClassConnRefused]; got != 1 {
		t.Errorf("by_class[connection_refused] = %d, want 1", got)
	}

	if len(body.ByProject) != 2 {
		t.Fatalf("by_project length = %d, want 2 (%+v)", len(body.ByProject), body.ByProject)
	}
	// Ordered by total desc: lane-a (3) then lane-b (1).
	if body.ByProject[0].Project != "lane-a" || body.ByProject[0].Total != 3 {
		t.Errorf("by_project[0] = %+v, want lane-a with total 3 (descending order)", body.ByProject[0])
	}
	if body.ByProject[1].Project != "lane-b" || body.ByProject[1].Total != 1 {
		t.Errorf("by_project[1] = %+v, want lane-b with total 1", body.ByProject[1])
	}
	if got := body.ByProject[0].ByClass[scheduler.GatewayErrClassRateLimited429]; got != 1 {
		t.Errorf("lane-a by_class[rate_limited_429] = %d, want 1", got)
	}
	if _, ok := body.ByProject[0].ByClass[scheduler.GatewayErrClassConnRefused]; !ok {
		t.Error("lane-a by_class is missing connection_refused — per-project classes must be closed too")
	}
}

// TestSCHEDGAP1654_GatewayErrorsEndpoint_EmptyWindowIsHonest — a window with
// no availability losses answers 200 with zeros and an empty ARRAY, never null
// and never a fabricated count.
func TestSCHEDGAP1654_GatewayErrorsEndpoint_EmptyWindowIsHonest(t *testing.T) {
	a := newAPITestServer(t)
	status, body, raw := schedGap1654Fetch(t, a.ts.URL)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body.Total != 0 {
		t.Errorf("total = %d on an empty window, want 0", body.Total)
	}
	if len(body.ByClass) != len(scheduler.GatewayErrorClassOrder) {
		t.Errorf("by_class has %d entries on an empty window, want %d (every class at 0)",
			len(body.ByClass), len(scheduler.GatewayErrorClassOrder))
	}
	if body.ByProject == nil {
		t.Error("by_project is nil on an empty window, want an empty array")
	}
	if !strings.Contains(raw, `"by_project":[]`) {
		t.Errorf("raw body %s does not serialize by_project as [] — null would make a consumer branch on absent data", raw)
	}
}

// TestSCHEDGAP1654_GatewayErrorsEndpoint_MethodNotAllowed — read-only surface.
func TestSCHEDGAP1654_GatewayErrorsEndpoint_MethodNotAllowed(t *testing.T) {
	a := newAPITestServer(t)
	resp, err := http.Post(a.ts.URL+"/api/v1/gateway-errors", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp.StatusCode)
	}
}

// TestSCHEDGAP1654_GatewayErrorsEndpoint_CountsRealSpawnLosses is the
// end-to-end half: a REAL Spawner (exec fallback disabled, event logger
// installed) driven against a mock gateway serving 503 and 429 writes the
// events this endpoint counts. It fails if either side of the contract drifts
// — a change to the emitted component/class vocabulary or to the handler's
// query breaks it, which is exactly the wiring a seeded-row test cannot see.
func TestSCHEDGAP1654_GatewayErrorsEndpoint_CountsRealSpawnLosses(t *testing.T) {
	cases := []struct {
		name    string
		project string
		status  int
		class   string
	}{
		{name: "503", project: "gap1654-api-503", status: http.StatusServiceUnavailable, class: scheduler.GatewayErrClassUnavailable503},
		{name: "429", project: "gap1654-api-429", status: http.StatusTooManyRequests, class: scheduler.GatewayErrClassRateLimited429},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			mustCreateAPITestProject(t, db, tc.project)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]string{"type": "gateway_error", "message": "mock refusal"},
				})
			}))
			t.Cleanup(srv.Close)

			const tickID = "gap1654-api-tick-1"
			now := time.Now().UTC().Format(time.RFC3339)
			if _, err := db.Exec(
				`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid) VALUES (?,?, 'running', ?,?, 0)`,
				tickID, tc.project, now, now); err != nil {
				t.Fatalf("insert running tick: %v", err)
			}

			spawner := scheduler.NewSpawner(db, 4)
			spawner.SetGatewayClient(scheduler.NewGatewayClient(srv.URL, "***", 5*time.Second))
			spawner.SetNoExecFallback(true)
			spawner.SetGatewayTransientRetries(0)
			spawner.SetEventLogger(scheduler.NewEventLogger(db))

			if _, err := spawner.Spawn(scheduler.PackedProject{Name: tc.project, Workdir: t.TempDir()}, tickID); err == nil {
				t.Fatalf("Spawn err = nil on HTTP %d, want the tick to be dropped", tc.status)
			}

			// The endpoint over the SAME DB the spawn path just wrote to.
			loop := scheduler.NewLoop(db, time.Minute, time.Hour, 10, 0, 5)
			t.Cleanup(loop.Stop)
			loop.SetNoExecFallback(true)
			apiSrv := api.NewServer(db, loop)
			ts := httptest.NewServer(apiSrv.Handler())
			t.Cleanup(ts.Close)

			status, body, raw := schedGap1654Fetch(t, ts.URL)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", status, raw)
			}
			if body.Total != 1 {
				t.Errorf("total = %d, want 1 (one dropped tick)", body.Total)
			}
			if got := body.ByClass[tc.class]; got != 1 {
				t.Errorf("by_class[%s] = %d, want 1 (classes: %+v)", tc.class, got, body.ByClass)
			}
			if len(body.ByProject) != 1 || body.ByProject[0].Project != tc.project {
				t.Fatalf("by_project = %+v, want exactly lane %s", body.ByProject, tc.project)
			}
			if got := body.ByProject[0].ByClass[tc.class]; got != 1 {
				t.Errorf("per-project by_class[%s] = %d, want 1", tc.class, got)
			}
		})
	}
}
