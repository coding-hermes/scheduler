// Package remote implements the REMOTE-005 control flow (docs/remote-spec.md
// §3): the primary→peer control client. Given a target peer id, the client
// sends a lane command to that peer's EXISTING HTTP API using the operator
// token (SCHED-GAP-1602) — no new protocol, no broker:
//
//	update  → PUT  /api/v1/projects/{name}
//	spawn   → POST /api/v1/projects/{name}/spawn
//	pause   → POST /api/v1/projects/{name}/pause
//	resume  → POST /api/v1/projects/{name}/resume
//
// THREE LAWS this package is built around:
//
//   - OWNERSHIP (§4): the primary COMMANDS and NEVER WRITES a peer's state.
//     The client performs exactly one local operation per command — the peer
//     URL lookup — and it is a READ. No peer row, project, tick or event is
//     ever mutated locally on a control call; the peer owns its records.
//   - FAIL-CLOSED + NON-DESTRUCTIVE (§3): every non-2xx answer and every
//     transport failure is SURFACED, never swallowed — the Result carries the
//     peer's status and response body verbatim (bounded), and the Go error is
//     non-nil for every non-success outcome. An unreachable peer is reported
//     as Unreachable, never as "down" (the §2 rendering law has no down state).
//   - AUTONOMY (§3 preamble): the client never touches the primary's own
//     scheduler state; a control failure is data for the caller (which logs
//     the correlation id and retries on the next poll), not a scheduling
//     dependency. The per-command deadline bounds how long a wedged peer can
//     hold the caller.
//
// Correlation logging: every command mints a correlation id following the
// REMOTE-004 convention (per-scheduler monotonic counter, scheduler-qualified
// — "counter + scheduler_id", never a random uuid, spec §8) and emits ONE
// structured JSON line per command:
//
//	{"corr_id":"...","peer":"...","method":"...","path":"...","status":200,"latency_ms":3,"error":""}
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coding-hermes/scheduler/internal/clock"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Command vocabulary (docs/remote-spec.md §3 control flow). The verbs map
// onto the peer's existing authenticated routes; nothing else is sent.
const (
	CommandUpdate = "update"
	CommandSpawn  = "spawn"
	CommandPause  = "pause"
	CommandResume = "resume"
)

// commandRoute maps a command to its HTTP method + path template. The path
// templates contain the documented {project} placeholder; render is the only
// place a concrete project name is substituted.
var commandRoute = map[string]struct {
	method string
	path   string
}{
	CommandUpdate: {"PUT", "/api/v1/projects/{project}"},
	CommandSpawn:  {"POST", "/api/v1/projects/{project}/spawn"},
	CommandPause:  {"POST", "/api/v1/projects/{project}/pause"},
	CommandResume: {"POST", "/api/v1/projects/{project}/resume"},
}

// maxBodyBytes bounds the verbatim response body kept on a Result — enough
// for the peer API's JSON error envelopes, small enough that a wedged or
// hostile peer cannot balloon the primary's memory through the log path.
const maxBodyBytes = 4 << 10 // 4 KiB

// commandTimeout bounds one control command. A control command must never
// stall the primary's own scheduling path (autonomy law): the deadline
// converts a wedged peer into a fast Unreachable result.
const commandTimeout = 10 * time.Second

// PeerLookup resolves a peer id to its API base URL. It is the ONLY local
// operation a control command performs, and implementations must be reads
// (ownership law §4: the primary never writes a peer's records). The wire-in
// for production is database.GetPeer — its ErrPeerNotFound is surfaced
// verbatim as the lookup error.
type PeerLookup func(ctx context.Context, peerID string) (baseURL string, err error)

// Result is one control command's structured outcome: the fields of the
// correlation log line plus the detail a caller needs to surface the failure
// (fail-closed). Status is the peer's HTTP status code — 0 when no response
// arrived (lookup failure or transport failure). Body is the peer's response
// body verbatim (bounded to maxBodyBytes).
type Result struct {
	CorrID    string `json:"corr_id"`
	Peer      string `json:"peer"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error"`
	// Unreachable distinguishes the transport arm (peer could not be
	// reached at all) from an answered failure. It is a transport
	// classification, never a peer health verdict: a peer that answers 503
	// has Unreachable=false, and an unreachable peer is "unreachable",
	// never "down" (§2 rendering law).
	Unreachable bool   `json:"unreachable"`
	Body        string `json:"body,omitempty"`
}

// ErrPeerUnreachable is wrapped by the error returned for a transport-level
// failure (dial, timeout, reset). errors.Is distinguishes the unreachable arm
// from an answered non-2xx (which returns a plain status error).
var ErrPeerUnreachable = errors.New("peer unreachable")

// Client sends control commands from the primary to a peer's existing HTTP
// API. Construct with NewClient; the zero value is not usable.
type Client struct {
	token string // shared operator token (SCHED-GAP-1602), sent as X-Operator-Token

	// lookup resolves peer id → base URL. A control command performs NO
	// other local access (ownership law).
	lookup PeerLookup

	httpClient *http.Client
	logw       io.Writer // correlation log sink, one JSON line per command
	schedID    string    // REMOTE-003 identity baked into every correlation id
	seq        atomic.Uint64

	// randID supplies per-command entropy so two commands from the same
	// process (same counter value is impossible, but ids must also be
	// unique across restarts) never collide in a merged log.
	randID func() string

	// clk is the component's time source (SCHED-GAP-169): zero value reads
	// as the wall clock, a test may install a simulator via SetClock.
	clk clock.Seam
}

// Option customizes a Client at construction.
type Option func(*Client)

// WithHTTPClient installs a custom *http.Client (tests: httptest stubs,
// injected transport failures). The client's Timeout is left as provided so
// tests can exercise the deadline arm deterministically.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithLogWriter installs the correlation-log sink. Default: os.Stderr.
func WithLogWriter(w io.Writer) Option {
	return func(c *Client) { c.logw = w }
}

// NewClient builds a control client. token is the operator credential shared
// with the peers (sent as X-Operator-Token, one of the three accepted forms
// of the SCHED-GAP-1602 gate); an empty token is a configuration error
// refuse-at-construction (fail-closed: a tokenless client could only ever
// produce 401s). schedulerID is the REMOTE-003 stable identity baked into
// every correlation id. lookup resolves peer ids — wire database.GetPeer.
func NewClient(token, schedulerID string, lookup PeerLookup, opts ...Option) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("remote control client: operator token must not be empty (peers fail closed behind SCHED-GAP-1602)")
	}
	if lookup == nil {
		return nil, errors.New("remote control client: peer lookup must not be nil")
	}
	c := &Client{
		token:      token,
		lookup:     lookup,
		httpClient: &http.Client{Timeout: commandTimeout},
		logw:       os.Stderr,
		schedID:    schedulerID,
		randID:     newRandID,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// UpdateProject sends PUT /api/v1/projects/{name} with patch marshalled as
// the JSON body (nil → empty body).
func (c *Client) UpdateProject(ctx context.Context, peerID, project string, patch any) (*Result, error) {
	return c.Control(ctx, peerID, project, CommandUpdate, patch)
}

// Spawn sends POST /api/v1/projects/{name}/spawn.
func (c *Client) Spawn(ctx context.Context, peerID, project string) (*Result, error) {
	return c.Control(ctx, peerID, project, CommandSpawn, nil)
}

// Pause sends POST /api/v1/projects/{name}/pause.
func (c *Client) Pause(ctx context.Context, peerID, project string) (*Result, error) {
	return c.Control(ctx, peerID, project, CommandPause, nil)
}

// Resume sends POST /api/v1/projects/{name}/resume.
func (c *Client) Resume(ctx context.Context, peerID, project string) (*Result, error) {
	return c.Control(ctx, peerID, project, CommandResume, nil)
}

// Control sends one lane command to the named peer. It ALWAYS returns a
// non-nil Result when it attempted the command (every field of the log line
// is populated, success or failure); the error is non-nil for every
// non-success outcome — unknown peer, transport failure, non-2xx status —
// and nil only on a 2xx. The failure surfaces are never swallowed:
//
//   - unknown peer → the lookup's error verbatim (ErrPeerNotFound passes
//     through errors.Is), Status=0, Unreachable=false (a lookup miss is a
//     configuration error, not a reachability verdict);
//   - transport failure → error wraps ErrPeerUnreachable, Unreachable=true,
//     Status=0;
//   - 4xx/5xx → error names the peer's status, Status set, Body verbatim
//     (bounded).
func (c *Client) Control(ctx context.Context, peerID, project, command string, body any) (*Result, error) {
	route, ok := commandRoute[command]
	if !ok {
		return nil, fmt.Errorf("remote control: unknown command %q (want update|spawn|pause|resume)", command)
	}
	if strings.TrimSpace(peerID) == "" {
		return nil, errors.New("remote control: peer id must not be empty")
	}
	if strings.TrimSpace(project) == "" {
		return nil, errors.New("remote control: project must not be empty")
	}

	res := &Result{
		CorrID: c.newCorrID(),
		Peer:   peerID,
		Method: route.method,
		Path:   strings.ReplaceAll(route.path, "{project}", project),
	}

	baseURL, err := c.lookup(ctx, peerID)
	if err != nil {
		res.Error = fmt.Sprintf("lookup: %v", err)
		c.logResult(res)
		// OWNERSHIP LAW: a control call that fails at lookup has written
		// nothing locally — the lookup itself is a read. Surface, never
		// swallow, and never fall back to a guessed URL.
		return res, fmt.Errorf("remote control %s %s peer %q: lookup: %w", res.Method, res.Path, peerID, err)
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		res.Error = "lookup: peer has an empty url"
		c.logResult(res)
		return res, fmt.Errorf("remote control %s %s peer %q: peer url is empty", res.Method, res.Path, peerID)
	}

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			res.Error = fmt.Sprintf("marshal body: %v", err)
			c.logResult(res)
			return res, fmt.Errorf("remote control %s %s: %w", res.Method, res.Path, err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, res.Method, baseURL+res.Path, rdr)
	if err != nil {
		res.Error = fmt.Sprintf("build request: %v", err)
		c.logResult(res)
		return res, fmt.Errorf("remote control %s %s: %w", res.Method, res.Path, err)
	}
	req.Header.Set("X-Operator-Token", c.token)
	req.Header.Set("X-Correlation-Id", res.CorrID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := c.clk.Get().Now()
	resp, err := c.httpClient.Do(req)
	res.LatencyMS = c.clk.Get().Since(start).Milliseconds()
	if err != nil {
		res.Error = fmt.Sprintf("unreachable: %v", err)
		res.Unreachable = true
		c.logResult(res)
		// AUTONOMY LAW: this is data for the caller (log + retry next
		// poll), never a scheduling dependency — but it is SURFACED as an
		// error, never swallowed.
		return res, fmt.Errorf("remote control %s %s peer %q: %w: %v", res.Method, res.Path, peerID, ErrPeerUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if readErr != nil {
		// A body read failure must not mask the status the peer DID send;
		// keep the status, note the truncation, and grade by status below.
		res.Body = fmt.Sprintf("<body read error: %v>", readErr)
	} else {
		res.Body = string(bodyBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		res.Error = fmt.Sprintf("peer %d: %s", resp.StatusCode, summarize(res.Body))
		c.logResult(res)
		return res, fmt.Errorf("remote control %s %s peer %q: peer returned %d (body verbatim on Result.Body)", res.Method, res.Path, peerID, resp.StatusCode)
	}

	c.logResult(res)
	return res, nil
}

// newCorrID mints a correlation id with the REMOTE-004 convention
// (spec §8): a per-scheduler monotonic counter qualified by the scheduler id
// — "counter + scheduler_id", never a random uuid — plus a short entropy
// suffix so ids stay unique across process restarts and merged logs.
func (c *Client) newCorrID() string {
	n := c.seq.Add(1)
	return fmt.Sprintf("ctrl-%s-%06d-%s", c.schedID, n, c.randID())
}

// newRandID is the default entropy source: 4 random bytes, hex-encoded.
func newRandID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is extraordinary; a zero-filled suffix keeps
		// the command moving (the counter prefix already disambiguates).
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// summarize condenses a response body for the log line's error field. The
// FULL verbatim body travels on Result.Body; the log line stays one line.
func summarize(body string) string {
	s := strings.TrimSpace(body)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// logResult emits the ONE structured correlation line for a command —
// exactly the documented shape {corr_id, peer, method, path, status,
// latency_ms, error}. A log sink failure never fails the command (the
// structured Result is already in the caller's hands; logging is the
// best-effort half).
func (c *Client) logResult(res *Result) {
	line := struct {
		CorrID    string `json:"corr_id"`
		Peer      string `json:"peer"`
		Method    string `json:"method"`
		Path      string `json:"path"`
		Status    int    `json:"status"`
		LatencyMS int64  `json:"latency_ms"`
		Error     string `json:"error"`
	}{
		CorrID:    res.CorrID,
		Peer:      res.Peer,
		Method:    res.Method,
		Path:      res.Path,
		Status:    res.Status,
		LatencyMS: res.LatencyMS,
		Error:     res.Error,
	}
	buf, err := json.Marshal(line)
	if err != nil {
		return
	}
	buf = append(buf, '\n')
	_, _ = c.logw.Write(buf)
}
