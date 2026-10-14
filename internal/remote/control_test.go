package remote

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// newLogCapture captures the correlation log lines the client writes.
type logCapture struct {
	mu atomic.Pointer[[]byte]
}

func newLogCapture() *logCapture {
	c := &logCapture{}
	c.mu.Store(&[]byte{})
	return c
}

// Write appends to the captured buffer (atomic so concurrent commands in
// future tests cannot tear a line).
func (l *logCapture) Write(p []byte) (int, error) {
	for {
		old := l.mu.Load()
		ne := append(append([]byte{}, *old...), p...)
		if l.mu.CompareAndSwap(old, &ne) {
			break
		}
	}
	return len(p), nil
}

func (l *logCapture) String() string { return string(*l.mu.Load()) }

func (l *logCapture) Lines() []string {
	var out []string
	for _, ln := range strings.Split(strings.TrimSpace(l.String()), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// staticLookup builds a PeerLookup over a fixed id→url table, mapping an
// unknown id to the registry's real ErrPeerNotFound.
func staticLookup(m map[string]string) PeerLookup {
	return func(_ context.Context, id string) (string, error) {
		if u, ok := m[id]; ok {
			return u, nil
		}
		return "", fmt.Errorf("%w: %s", database.ErrPeerNotFound, id)
	}
}

func newTestClient(t *testing.T, lookup PeerLookup, logw io.Writer) *Client {
	t.Helper()
	c, err := NewClient("op-secret-token", "primary-a", lookup, WithLogWriter(logw))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// corrLine is the documented correlation log line shape.
type corrLine struct {
	CorrID    string `json:"corr_id"`
	Peer      string `json:"peer"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error"`
}

func parseCorrLine(t *testing.T, raw string) corrLine {
	t.Helper()
	var ln corrLine
	if err := json.Unmarshal([]byte(raw), &ln); err != nil {
		t.Fatalf("correlation log line is not valid JSON: %v\nline: %s", err, raw)
	}
	return ln
}

// openOwnershipDB opens a REAL file-backed scheduler database (the daemon's
// own InitDB: pragmas + migrations), then switches it to rollback-journal
// mode. WAL would hide committed writes in a sidecar -wal file and defeat
// the whole-file hash the ownership proof leans on; journal_mode=DELETE
// folds every commit into the main file and removes the journal on commit,
// so sha256(db file) is a sound write detector.
func openOwnershipDB(t *testing.T) (db *sql.DB, dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "ownership.db")
	d, err := database.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if _, err := d.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("switch journal_mode=DELETE: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, dbPath
}

// sha256File hashes a file's full content.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// peerRowImage renders the FULL peer row as one comparable string — every
// column the registry carries, so any mutation of any field breaks equality.
func peerRowImage(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var img string
	err := db.QueryRowContext(context.Background(),
		`SELECT id || '|' || url || '|' || COALESCE(version,'') || '|' || COALESCE(capabilities,'') || '|' ||
		        COALESCE(last_contact,'') || '|' || registered_at || '|' || updated_at
		 FROM peers WHERE id = ?`, id).Scan(&img)
	if err != nil {
		t.Fatalf("peer row image %q: %v", id, err)
	}
	return img
}

// TestControlSendsDocumentedMethodPathToken is ACCEPTANCE 2: an httptest
// peer stub proves the client sends the documented method/path with the
// operator token, the correlation header, and the correlation id in the log
// line — across all four commands of the §3 route table.
func TestControlSendsDocumentedMethodPathToken(t *testing.T) {
	type seen struct {
		method, path, token, corr string
		body                      []byte
	}
	var got atomic.Pointer[seen]
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(&seen{
			method: r.Method,
			path:   r.URL.Path,
			token:  r.Header.Get("X-Operator-Token"),
			corr:   r.Header.Get("X-Correlation-Id"),
			body:   b,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer peer.Close()

	logs := newLogCapture()
	c := newTestClient(t, staticLookup(map[string]string{"peer-b": peer.URL}), logs)

	cases := []struct {
		command string
		do      func() (*Result, error)
		method  string
		path    string
		body    string
	}{
		{"update", func() (*Result, error) {
			return c.UpdateProject(context.Background(), "peer-b", "crier", map[string]any{"enabled": false})
		}, "PUT", "/api/v1/projects/crier", `{"enabled":false}`},
		{"spawn", func() (*Result, error) { return c.Spawn(context.Background(), "peer-b", "crier") }, "POST", "/api/v1/projects/crier/spawn", ""},
		{"pause", func() (*Result, error) { return c.Pause(context.Background(), "peer-b", "crier") }, "POST", "/api/v1/projects/crier/pause", ""},
		{"resume", func() (*Result, error) { return c.Resume(context.Background(), "peer-b", "crier") }, "POST", "/api/v1/projects/crier/resume", ""},
	}

	for i, tc := range cases {
		res, err := tc.do()
		if err != nil {
			t.Fatalf("case %s: unexpected error: %v", tc.command, err)
		}
		if res.Status != http.StatusOK {
			t.Fatalf("case %s: status = %d, want 200", tc.command, res.Status)
		}
		s := got.Load()
		if s.method != tc.method || s.path != tc.path {
			t.Fatalf("case %s: wire saw %s %s, want %s %s", tc.command, s.method, s.path, tc.method, tc.path)
		}
		if s.token != "op-secret-token" {
			t.Fatalf("case %s: peer did not receive the operator token (X-Operator-Token=%q)", tc.command, s.token)
		}
		if s.corr == "" {
			t.Fatalf("case %s: X-Correlation-Id header missing on the wire", tc.command)
		}
		if tc.body == "" && len(s.body) != 0 {
			t.Fatalf("case %s: unexpected body %q", tc.command, s.body)
		}
		if tc.body != "" && string(s.body) != tc.body {
			t.Fatalf("case %s: body = %s, want %s", tc.command, s.body, tc.body)
		}

		// ACCEPTANCE 2 (correlation id present in the log line): exactly one
		// line per command, carrying THIS command's correlation id.
		lines := logs.Lines()
		if len(lines) != i+1 {
			t.Fatalf("case %s: %d log lines so far, want %d", tc.command, len(lines), i+1)
		}
		ln := parseCorrLine(t, lines[len(lines)-1])
		if ln.CorrID != s.corr {
			t.Fatalf("case %s: log corr_id %q != wire X-Correlation-Id %q", tc.command, ln.CorrID, s.corr)
		}
		if !strings.HasPrefix(ln.CorrID, "ctrl-primary-a-") {
			t.Fatalf("case %s: correlation id %q does not follow the REMOTE-004 scheduler-qualified convention", tc.command, ln.CorrID)
		}
		if ln.Peer != "peer-b" || ln.Method != tc.method || ln.Path != tc.path || ln.Status != 200 || ln.Error != "" {
			t.Fatalf("case %s: log line %+v does not match the command", tc.command, ln)
		}
		if ln.LatencyMS < 0 {
			t.Fatalf("case %s: negative latency %d", tc.command, ln.LatencyMS)
		}
	}
}

// TestControlCorrIDsUniqueAndCounterQualified pins the REMOTE-004 convention:
// per-scheduler monotonic counter + scheduler id ("counter + scheduler_id",
// never a random uuid — spec §8), unique across commands.
func TestControlCorrIDsUniqueAndCounterQualified(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer peer.Close()

	logs := newLogCapture()
	c := newTestClient(t, staticLookup(map[string]string{"p": peer.URL}), logs)
	for i := 0; i < 3; i++ {
		if _, err := c.Pause(context.Background(), "p", "proj"); err != nil {
			t.Fatalf("pause %d: %v", i, err)
		}
	}
	lines := logs.Lines()
	ids := make([]string, 0, len(lines))
	for _, ln := range lines {
		ids = append(ids, parseCorrLine(t, ln).CorrID)
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("correlation ids not unique: %v", ids)
	}
	if !strings.HasPrefix(ids[0], "ctrl-primary-a-000001-") || !strings.HasPrefix(ids[2], "ctrl-primary-a-000003-") {
		t.Fatalf("counter prefix not monotonic/qualified: %v", ids)
	}
}

// TestControlSurfacesPeerError is ACCEPTANCE 3a: a peer 4xx/5xx is surfaced
// — never swallowed — with the peer's status AND body verbatim in the log
// line and the returned Result.
func TestControlSurfacesPeerError(t *testing.T) {
	peerBody := `{"error":"lane is enabled (409 semantics live elsewhere)"}`
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(peerBody))
		}))
		logs := newLogCapture()
		c := newTestClient(t, staticLookup(map[string]string{"p": peer.URL}), logs)

		res, err := c.Pause(context.Background(), "p", "proj")
		if err == nil {
			t.Fatalf("status %d: error swallowed — control failure must surface", code)
		}
		if res == nil {
			t.Fatalf("status %d: Result must still be returned", code)
		}
		if res.Status != code {
			t.Fatalf("status %d: Result.Status = %d", code, res.Status)
		}
		if res.Body != peerBody {
			t.Fatalf("status %d: body not verbatim: %q", code, res.Body)
		}
		if res.Unreachable {
			t.Fatalf("status %d: an ANSWERED failure must not be classified unreachable", code)
		}
		if !strings.Contains(err.Error(), "peer returned") {
			t.Fatalf("status %d: error does not name the peer status: %v", code, err)
		}
		// The log line carries the peer's status + body verbatim.
		ln := parseCorrLine(t, logs.Lines()[0])
		if ln.Status != code || ln.Error == "" {
			t.Fatalf("status %d: log line does not surface the failure: %+v", code, ln)
		}
		if !strings.Contains(ln.Error, "lane is enabled") {
			t.Fatalf("status %d: log error does not carry the peer body: %+v", code, ln)
		}
		peer.Close()
	}
}

// TestControlUnreachable is ACCEPTANCE 3b: a transport failure is reported as
// unreachable (ErrPeerUnreachable, Unreachable=true, Status=0), never as
// "down" — the §2 rendering law has no down state.
func TestControlUnreachable(t *testing.T) {
	// A closed port: dial fails fast for every attempt.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := peer.URL
	peer.Close()

	logs := newLogCapture()
	c := newTestClient(t, staticLookup(map[string]string{"p": url}), logs)

	res, err := c.Spawn(context.Background(), "p", "proj")
	if err == nil {
		t.Fatal("transport failure swallowed")
	}
	if !errors.Is(err, ErrPeerUnreachable) {
		t.Fatalf("error does not wrap ErrPeerUnreachable: %v", err)
	}
	if !res.Unreachable || res.Status != 0 {
		t.Fatalf("Result: unreachable=%v status=%d, want true/0", res.Unreachable, res.Status)
	}
	if strings.Contains(strings.ToLower(err.Error()), "down") {
		t.Fatalf(`unreachable peer must not be reported as "down": %v`, err)
	}
	ln := parseCorrLine(t, logs.Lines()[0])
	if ln.Status != 0 || ln.Error == "" {
		t.Fatalf("log line does not surface the unreachable arm: %+v", ln)
	}
}

// TestControlUnknownPeerFailsClosed: an unregistered peer id surfaces the
// registry's error verbatim (ErrPeerNotFound passes errors.Is) — it is a
// configuration error, NOT a reachability verdict.
func TestControlUnknownPeerFailsClosed(t *testing.T) {
	logs := newLogCapture()
	c := newTestClient(t, staticLookup(map[string]string{}), logs)

	res, err := c.Pause(context.Background(), "ghost", "proj")
	if !errors.Is(err, database.ErrPeerNotFound) {
		t.Fatalf("want ErrPeerNotFound verbatim, got: %v", err)
	}
	if res == nil || res.Unreachable {
		t.Fatalf("lookup miss must not be classified unreachable: %+v", res)
	}
	ln := parseCorrLine(t, logs.Lines()[0])
	if ln.Status != 0 || !strings.Contains(ln.Error, "peer not found") {
		t.Fatalf("log line does not surface the lookup failure: %+v", ln)
	}
}

// TestControlOwnershipAndAutonomy proves BOTH halves of the §4/§3 law on a
// real file-backed scheduler DB (the daemon's own InitDB + migrations),
// sharing one database so the expensive part — the 57-migration boot — runs
// once. journal_mode=DELETE (see openOwnershipDB) makes the whole-file
// sha256 a sound write detector: WAL would hide committed writes in a
// sidecar -wal file; DELETE mode folds every commit into the main file.
//
// The production lookup — database.GetPeer, a READ — is wired exactly as
// main.go would; by the ownership law it is the ONLY local operation a
// control command performs.
func TestControlOwnershipAndAutonomy(t *testing.T) {
	ctx := context.Background()
	db, dbPath := openOwnershipDB(t)

	// A closed high port: dial refused instantly (dials to :1 stall in
	// sandboxed environments — measured 7s each).
	deadPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := deadPeer.URL
	deadPeer.Close()

	c, err := NewClient("tok", "primary-a", func(ctx context.Context, id string) (string, error) {
		p, err := database.GetPeer(ctx, db, id)
		if err != nil {
			return "", err
		}
		return p.URL, nil
	}, WithLogWriter(io.Discard))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// ACCEPTANCE 4: NO local write to the target peer's row occurs on a
	// control call (ownership law §4). Register the peer, snapshot the FULL
	// row plus the whole database file, run update+spawn+pause+resume
	// against the unreachable peer, assert zero mutation: row identical,
	// one row total, file bytes identical — and failures trigger no local
	// compensation write either.
	t.Run("no_local_write_to_peer_row", func(t *testing.T) {
		if err := database.UpsertPeer(ctx, db, &database.Peer{
			ID: "peer-b", URL: deadURL, Version: "v1", Capabilities: "control,query",
		}); err != nil {
			t.Fatalf("upsert peer: %v", err)
		}
		// Detector soundness (anti-vacuity): the whole-file hash MUST move
		// on a real registry write, otherwise the zero-write assertion
		// below could pass on a broken detector. One heartbeat — a real
		// write — flips the hash; then the proof proper starts from the
		// post-write state.
		h1 := sha256File(t, dbPath)
		if err := database.PeerHeartbeat(ctx, db, "peer-b"); err != nil {
			t.Fatalf("heartbeat (detector probe): %v", err)
		}
		if h2 := sha256File(t, dbPath); h1 == h2 {
			t.Fatal("detector unsound: a real write did not change the database file hash — the zero-write proof would be vacuous")
		}
		rowBefore := peerRowImage(t, db, "peer-b")
		before := sha256File(t, dbPath)

		for _, run := range []func() error{
			func() error {
				_, err := c.UpdateProject(ctx, "peer-b", "proj", map[string]any{"enabled": false})
				return err
			},
			func() error { _, err := c.Spawn(ctx, "peer-b", "proj"); return err },
			func() error { _, err := c.Pause(ctx, "peer-b", "proj"); return err },
			func() error { _, err := c.Resume(ctx, "peer-b", "proj"); return err },
		} {
			if err := run(); !errors.Is(err, ErrPeerUnreachable) {
				t.Fatalf("expected unreachable failure, got: %v", err)
			}
		}

		if rowAfter := peerRowImage(t, db, "peer-b"); rowBefore != rowAfter {
			t.Fatalf("OWNERSHIP VIOLATION: peer row mutated on control calls\nbefore: %q\nafter:  %q", rowBefore, rowAfter)
		}
		peers, err := database.ListPeers(ctx, db)
		if err != nil || len(peers) != 1 {
			t.Fatalf("peer table changed: %d rows (err=%v)", len(peers), err)
		}
		if after := sha256File(t, dbPath); before != after {
			t.Fatalf("OWNERSHIP VIOLATION: database file changed on control calls (before %s, after %s)", before, after)
		}
	})

	// The autonomy half: a failing control command must not write ANY of
	// the primary's own scheduling tables. A local project row exists (a
	// real local write by the seed path), the control command fails, and
	// the whole database file is byte-identical afterwards.
	t.Run("failed_command_does_not_touch_scheduler_state", func(t *testing.T) {
		if err := database.UpsertPeer(ctx, db, &database.Peer{ID: "far", URL: deadURL}); err != nil {
			t.Fatalf("upsert peer: %v", err)
		}
		if err := database.CreateProject(ctx, db, &database.Project{
			Weight: 10, Priority: 5, Name: "local-lane",
			RepoURL: "https://example.com/local", Workdir: "/tmp/local-lane",
		}); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		before := sha256File(t, dbPath)

		if _, err := c.Spawn(ctx, "far", "local-lane"); !errors.Is(err, ErrPeerUnreachable) {
			t.Fatalf("expected unreachable failure, got: %v", err)
		}
		if after := sha256File(t, dbPath); before != after {
			t.Fatalf("AUTONOMY VIOLATION: failed control command wrote to the primary's database (before %s, after %s)", before, after)
		}
	})
}

// TestNewClientRefusesTokenless pins the fail-closed constructor: without an
// operator token the client could only ever produce 401s, so it refuses to
// build at all.
func TestNewClientRefusesTokenless(t *testing.T) {
	if _, err := NewClient("", "primary-a", staticLookup(map[string]string{})); err == nil {
		t.Fatal("tokenless client must be refused at construction")
	}
	if _, err := NewClient("tok", "primary-a", nil); err == nil {
		t.Fatal("nil lookup must be refused at construction")
	}
}
