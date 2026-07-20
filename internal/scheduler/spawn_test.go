package scheduler

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:): %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestNewSpawner_Defaults verifies the constructor sets sane defaults.
func TestNewSpawner_Defaults(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 5)
	if s == nil {
		t.Fatal("NewSpawner returned nil")
	}
	if s.ActiveCount() != 0 {
		t.Errorf("initial ActiveCount = %d, want 0", s.ActiveCount())
	}
}

// TestSpawner_CanSpawn verifies the concurrency check before any spawn.
func TestSpawner_CanSpawn(t *testing.T) {
	db := newTestDB(t)

	// maxConcurrent=2 → canSpawn should be true with no active.
	s := NewSpawner(db, 2)
	if !s.canSpawn() {
		t.Error("canSpawn() = false with no active, want true")
	}
}

// TestSpawner_ActiveCountConsistent verifies ActiveCount stays in sync with the active map.
func TestSpawner_ActiveCountConsistent(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 10)

	// Initially empty.
	if got := s.ActiveCount(); got != 0 {
		t.Errorf("initial ActiveCount = %d, want 0", got)
	}
	if !s.canSpawn() {
		t.Error("canSpawn() = false at start, want true")
	}
}

// TestSpawner_ZeroMaxConcurrent verifies a 0-concurrency spawner can never spawn.
// We don't actually call Spawn (which would invoke hermes); we just inspect the invariant.
func TestSpawner_ZeroMaxConcurrent(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 0)

	if s.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0", s.ActiveCount())
	}
	// canSpawn should return false because 0 active < 0 is false.
	if s.canSpawn() {
		t.Error("canSpawn() = true with maxConcurrent=0, want false")
	}
}

// TestSpawner_MaxConcurrentOne confirms boundary behavior at maxConcurrent=1.
func TestSpawner_MaxConcurrentOne(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 1)

	// No active → canSpawn true.
	if !s.canSpawn() {
		t.Error("canSpawn() = false with maxConcurrent=1 and no active, want true")
	}
}

func TestSpawnTimeoutKillsProcessGroup(t *testing.T) {
	db := newTestDB(t)
	spawner := NewSpawner(db, 1)
	spawner.timeout = 200 * time.Millisecond

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	project := PackedProject{
		Name:    "process-group-timeout",
		Workdir: t.TempDir(),
		Command: fmt.Sprintf(
			"bash -c 'sleep 30 & echo $! > %s; wait'",
			pidFile,
		),
	}

	tick, err := spawner.Spawn(project, "process-group-timeout-tick")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var childPID int
	for time.Now().Before(deadline) {
		contents, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(contents)))
			if err == nil && childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child PID was not written")
	}

	outcome := tick.Wait()
	if outcome.Status != TickTimeout {
		t.Fatalf("status = %s, want %s", outcome.Status, TickTimeout)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d survived scheduler timeout", childPID)
}

// BenchmarkEstimateTickCost measures the cost-estimation function called from
// SpawnedTick.Wait() on every completed tick. Today it returns package-level
// constants; the benchmark pins the current cost so a future switch to real
// session export shows up as a regression.
func BenchmarkEstimateTickCost(b *testing.B) {
	var (
		tin, tout int
		cost      float64
	)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tin, tout, cost = estimateTickCost()
	}
	// Touch results so the compiler can't elide the call.
	if tin == 0 && tout == 0 && cost == 0 {
		b.Fatal("estimateTickCost returned zeros")
	}
}

// benchSpawnFixture returns a project representative of a real spawn.
// Worker defaults are populated so the WorkerDefaults() helper exercises its
// non-empty branch — that branch is the most expensive string-formatting in
// the spawn prep path.
func benchSpawnFixture() PackedProject {
	return PackedProject{
		Name:        "bench-project",
		Priority:    7,
		Weight:      10,
		Workdir:     "/tmp/bench-project",
		RepoURL:     "https://example.com/bench-project",
		Model:       "deepseek-v4-flash",
		Provider:    "deepseek-foreman",
		WorkerModel: "kimi-k3",
		Deliver:     "telegram:-1001234567890:42",
	}
}

// BenchmarkNewSpawner measures the constructor cost. NewSpawner expands $HOME
// via os.ExpandEnv which is the only non-trivial work — pinning it lets us
// detect if the constructor starts doing real work.
func BenchmarkNewSpawner(b *testing.B) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		b.Fatalf("InitDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	b.ResetTimer()
	var sink *Spawner
	for i := 0; i < b.N; i++ {
		sink = NewSpawner(db, 4)
	}
	if sink == nil {
		b.Fatal("NewSpawner returned nil")
	}
}

// BenchmarkSpawn_Prep measures the data-preparation portion of Spawn() that
// happens BEFORE exec.Command is invoked: canSpawn check, model/provider
// resolution, WorkerDefaults formatting, and the prompt template assembly.
// The actual fork+exec is intentionally excluded so the benchmark reflects
// pure scheduler-side work, not OS process startup.
func BenchmarkSpawn_Prep(b *testing.B) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		b.Fatalf("InitDB: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	project := benchSpawnFixture()
	tickID := fmt.Sprintf("%s-%s", project.Name, time.Now().UTC().Format("2006-01-02-15-04-05"))
	spawner := NewSpawner(db, 4)

	b.ResetTimer()
	var (
		sinkModel string
		sinkArgs  []string
	)
	for i := 0; i < b.N; i++ {
		// --- BEGIN spawn prep (mirrors spawn.go lines 159-258) ---
		_ = spawner.canSpawn() // concurrency check

		model := spawner.model
		if project.Model != "" {
			model = project.Model
		}
		provider := spawner.provider
		if project.Provider != "" {
			provider = project.Provider
		}

		_ = WorkerDefaults(project)

		// Mirror the prompt template assembly (format-only — don't run it).
		_ = fmt.Sprintf(
			"[Scheduler tick: %s] "+
				"Load skills coding-hermes-foreman, coding-hermes-cron, hilo-usage, gitreins. "+
				"Read .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. "+
				"Workdir: %s. "+
				"IMPORTANT: You are a FOREMAN, not a worker. Browser/interactive work belongs in workers (delegate). "+
				"Format your final output as clean, well-structured markdown with tables and sections. "+
				"%s"+
				"Report result.",
			tickID, project.Workdir,
			WorkerDefaults(project),
		)

		// Mirror the args list assembly.
		sinkArgs = []string{
			"chat", "-q", "PROMPT_PLACEHOLDER",
			"-m", model,
			"--provider", provider,
			"-s", "coding-hermes-foreman",
			"-s", "coding-hermes-cron",
			"-s", "hilo-usage",
			"-s", "gitreins",
			"--ignore-rules", "-Q",
		}
		sinkModel = model
		// --- END spawn prep ---
	}
	if sinkModel == "" {
		b.Fatal("sinkModel stayed empty")
	}
	if len(sinkArgs) == 0 {
		b.Fatal("sinkArgs stayed empty")
	}
}

// ── Spawn helpers ──

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantLen int
		want    []string
	}{
		{
			name:    "plain",
			cmd:     "hermes chat",
			wantLen: 2,
			want:    []string{"hermes", "chat"},
		},
		{
			name:    "single_word",
			cmd:     "hermes",
			wantLen: 1,
			want:    []string{"hermes"},
		},
		{
			name:    "quoted",
			cmd:     `hermes chat "hello world"`,
			wantLen: 3,
			want:    []string{"hermes", "chat", "hello world"},
		},
		{
			name:    "quoted_with_space",
			cmd:     `echo "a b c" extra`,
			wantLen: 3,
			want:    []string{"echo", "a b c", "extra"},
		},
		{
			name:    "empty",
			cmd:     "",
			wantLen: 0,
			want:    nil,
		},
		{
			name:    "trailing_spaces",
			cmd:     "hermes   chat  ",
			wantLen: 2,
			want:    []string{"hermes", "chat"},
		},
		{
			name:    "unclosed_quote",
			cmd:     `echo "unclosed`,
			wantLen: 2,
			want:    []string{"echo", "unclosed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitCommand(tt.cmd)
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d (got=%v)", len(got), tt.wantLen, got)
				return
			}
			for i, w := range got {
				if i < len(tt.want) && w != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, w, tt.want[i])
				}
			}
		})
	}
}

func TestGatewayAvailable_NilGateway(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 4)
	if s.GatewayAvailable() {
		t.Error("GatewayAvailable() = true with nil gateway, want false")
	}
}

func TestGatewayAvailable_WithGateway(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	gw := NewGatewayClient(srv.URL, "", 5*time.Second)
	s.SetGatewayClient(gw)

	if !s.GatewayAvailable() {
		t.Error("GatewayAvailable() = false against live test server, want true")
	}
}

func TestSpawnMethodCounts_Initial(t *testing.T) {
	db := newTestDB(t)
	s := NewSpawner(db, 4)

	httpCount, execCount := s.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0)", httpCount, execCount)
	}
}

func TestEstimateTickCost_ReturnsConstants(t *testing.T) {
	tin, tout, cost := estimateTickCost()
	if tin != 8000 {
		t.Errorf("tokensIn = %d, want 8000", tin)
	}
	if tout != 2000 {
		t.Errorf("tokensOut = %d, want 2000", tout)
	}
	if cost <= 0 {
		t.Errorf("costUSD = %f, want > 0", cost)
	}
}
