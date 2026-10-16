package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	schedulerclock "github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/connector"
)

const schemaVersion = "0.152.0"

// Clock is the adapter's time seam.
type Clock interface{ Now() time.Time }

// Supervisor proves whether the native process tree has reached zero descendants.
type Supervisor interface {
	WaitZeroDescendants(context.Context, connector.Correlation) (bool, error)
}

// Collector turns native/workspace evidence into durable references and digests.
type Collector interface {
	Collect(context.Context, connector.CollectRequest) (connector.ArtifactBundle, error)
}

// Options configures a testable adapter. Transport must already represent the
// sidecar's stdio; this package never launches a process.
type Options struct {
	Transport                                     io.ReadWriteCloser
	Supervisor                                    Supervisor
	Collector                                     Collector
	Clock                                         Clock
	MaxLineBytes, MaxPendingRequests, EventBuffer int
	Capabilities                                  connector.Capabilities
}

// Adapter implements connector.Connector for Codex app-server 0.152.0.
type Adapter struct {
	rpc                   *rpcClient
	supervisor            Supervisor
	collector             Collector
	clock                 Clock
	caps                  connector.Capabilities
	mu                    sync.Mutex
	initialized, disposed bool
	corr                  connector.Correlation
	actual                connector.ActualIdentity
	active                bool
	terminal              connector.TerminalStatus
	threadStatus          string
	terminalAt            time.Time
	seq                   uint64
	events                chan connector.Event
	changed               chan struct{}
	deferred              []deferredNotification
}

type deferredNotification struct {
	method string
	params json.RawMessage
}

// New constructs an unwired adapter over an injected transport.
func New(o Options) (*Adapter, error) {
	if o.Transport == nil {
		return nil, errors.New("codex app-server: transport is required")
	}
	if o.MaxLineBytes <= 0 {
		o.MaxLineBytes = 1 << 20
	}
	if o.MaxPendingRequests <= 0 {
		o.MaxPendingRequests = 32
	}
	if o.EventBuffer <= 0 {
		o.EventBuffer = 64
	}
	if o.Clock == nil {
		o.Clock = schedulerclock.Real()
	}
	a := &Adapter{supervisor: o.Supervisor, collector: o.Collector, clock: o.Clock, caps: o.Capabilities, events: make(chan connector.Event, o.EventBuffer), changed: make(chan struct{})}
	a.rpc = newRPC(o.Transport, o.MaxLineBytes, o.MaxPendingRequests, a.notification)
	return a, nil
}

// Describe describes this pinned native adapter.
func (a *Adapter) Describe(context.Context) (connector.Description, error) {
	return connector.Description{ID: "codex-app-server", Version: schemaVersion, NativeProtocol: "codex-app-server-jsonrpc-v2", Capabilities: a.caps}, nil
}

func (a *Adapter) ensureInitialized(ctx context.Context) error {
	a.mu.Lock()
	if a.initialized {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	var out struct{ UserAgent, CodexHome, PlatformFamily, PlatformOS string }
	err := a.rpc.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "coding-hermes-scheduler", "version": "dark-connector"}, "capabilities": map[string]any{"experimentalApi": false}}, &out)
	if err != nil {
		return wrap("initialize", connector.ErrorTransport, err)
	}
	if out.UserAgent == "" || out.CodexHome == "" || out.PlatformFamily == "" || out.PlatformOS == "" {
		return wrap("initialize", connector.ErrorProtocol, errors.New("incomplete initialize response"))
	}
	if err := a.rpc.notify("initialized", map[string]any{}); err != nil {
		return wrap("initialize", connector.ErrorTransport, err)
	}
	a.mu.Lock()
	a.initialized = true
	a.mu.Unlock()
	return nil
}

// Preflight performs account/read and exhausts model/list pagination.
func (a *Adapter) Preflight(ctx context.Context, r connector.PreflightRequest) (connector.PreflightReceipt, error) {
	if err := validCorrelation(r.Correlation, false); err != nil {
		return connector.PreflightReceipt{}, err
	}
	if err := a.ensureInitialized(ctx); err != nil {
		return connector.PreflightReceipt{}, err
	}
	var acct struct {
		Account  json.RawMessage `json:"account"`
		Requires bool            `json:"requiresOpenaiAuth"`
	}
	if err := a.rpc.call(ctx, "account/read", map[string]any{"refreshToken": false}, &acct); err != nil {
		return connector.PreflightReceipt{}, wrap("preflight", connector.ErrorTransport, err)
	}
	auth := !acct.Requires || string(acct.Account) != "null"
	cursor := ""
	found := false
	checked := 0
	seen := map[string]bool{}
	for {
		var page struct {
			Data []struct {
				Model string `json:"model"`
			} `json:"data"`
			Next *string `json:"nextCursor"`
		}
		p := map[string]any{"includeHidden": false, "limit": 100}
		if cursor != "" {
			p["cursor"] = cursor
		}
		if err := a.rpc.call(ctx, "model/list", p, &page); err != nil {
			return connector.PreflightReceipt{}, wrap("preflight", connector.ErrorTransport, err)
		}
		for _, m := range page.Data {
			checked++
			if m.Model == r.Correlation.Requested.Model {
				found = true
			}
		}
		if page.Next == nil || *page.Next == "" {
			break
		}
		if seen[*page.Next] {
			return connector.PreflightReceipt{}, wrap("preflight", connector.ErrorProtocol, errors.New("pagination cursor cycle"))
		}
		seen[*page.Next] = true
		cursor = *page.Next
	}
	return connector.PreflightReceipt{Correlation: r.Correlation, Ready: auth && found, Authenticated: auth, ModelAvailable: found, ModelsChecked: checked}, nil
}

// Start creates a thread in the leased cwd, then starts its first turn.
func (a *Adapter) Start(ctx context.Context, r connector.StartRequest) (connector.StartResult, error) {
	if err := validCorrelation(r.Correlation, false); err != nil {
		return connector.StartResult{}, err
	}
	if r.WorkspacePath == "" || !strings.HasPrefix(r.WorkspacePath, "/") {
		return connector.StartResult{}, wrap("start", connector.ErrorInvalid, errors.New("workspace path must be absolute"))
	}
	if err := a.ensureInitialized(ctx); err != nil {
		return connector.StartResult{}, err
	}
	instructions := renderBrief(r.Brief)
	var ts struct {
		Model         string          `json:"model"`
		ModelProvider string          `json:"modelProvider"`
		CWD           string          `json:"cwd"`
		Approval      json.RawMessage `json:"approvalPolicy"`
		Sandbox       json.RawMessage `json:"sandbox"`
		Thread        struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	p := map[string]any{"cwd": r.WorkspacePath, "model": r.Correlation.Requested.Model, "modelProvider": r.Correlation.Requested.Provider, "sandbox": "workspace-write", "approvalPolicy": "never", "baseInstructions": instructions, "ephemeral": false}
	if err := a.rpc.call(ctx, "thread/start", p, &ts); err != nil {
		return connector.StartResult{}, wrap("start", connector.ErrorTransport, err)
	}
	if ts.Thread.ID == "" || ts.Model == "" || ts.ModelProvider == "" || ts.CWD != r.WorkspacePath || len(ts.Approval) == 0 || len(ts.Sandbox) == 0 {
		return connector.StartResult{}, wrap("start", connector.ErrorProtocol, errors.New("incomplete or mismatched thread/start response"))
	}
	c := r.Correlation
	c.NativeSessionID = ts.Thread.ID
	var tr struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	tr.Turn.Status = ""
	if err := a.rpc.call(ctx, "turn/start", map[string]any{"threadId": ts.Thread.ID, "input": []any{map[string]any{"type": "text", "text": instructions}}}, &tr); err != nil {
		return connector.StartResult{}, wrap("start", connector.ErrorTransport, err)
	}
	if tr.Turn.ID == "" {
		return connector.StartResult{}, wrap("start", connector.ErrorProtocol, errors.New("turn/start response missing turn id"))
	}
	c.NativeTurnID = tr.Turn.ID
	actual := connector.ActualIdentity{Model: ts.Model, Provider: ts.ModelProvider, EvidenceSource: "thread/start response", ObservedAt: a.clock.Now()}
	a.mu.Lock()
	if a.corr.RunID != "" {
		a.mu.Unlock()
		return connector.StartResult{}, wrap("start", connector.ErrorInvalid, errors.New("adapter already owns an attempt"))
	}
	a.corr = c
	a.actual = actual
	a.active = true
	a.threadStatus = "active"
	a.emitLocked(connector.EventStarted, connector.TerminalNone, &actual, "native thread and turn started")
	deferred := a.deferred
	a.deferred = nil
	for _, n := range deferred {
		if err := a.notificationLocked(n.method, n.params); err != nil {
			a.mu.Unlock()
			a.rpc.fail(err)
			return connector.StartResult{}, wrap("start", connector.ErrorProtocol, err)
		}
	}
	a.mu.Unlock()
	return connector.StartResult{Correlation: c, Actual: actual}, nil
}

type stream struct {
	a    *Adapter
	corr connector.Correlation
}

func (s *stream) Next(ctx context.Context) (connector.Event, error) {
	select {
	case e := <-s.a.events:
		if !same(s.corr, e.Correlation) {
			return connector.Event{}, wrap("events", connector.ErrorProtocol, errors.New("event correlation mismatch"))
		}
		return e, nil
	case <-ctx.Done():
		return connector.Event{}, ctx.Err()
	case <-s.a.rpc.done:
		return connector.Event{}, io.EOF
	}
}

// Events opens the adapter's single bounded ordered event stream.
func (a *Adapter) Events(_ context.Context, c connector.Correlation) (connector.EventStream, error) {
	if !a.caps.Events {
		return nil, unsupported("events")
	}
	if err := a.match(c); err != nil {
		return nil, err
	}
	return &stream{a: a, corr: c}, nil
}

// Get returns native observation only, never verification authority.
func (a *Adapter) Get(_ context.Context, c connector.Correlation) (connector.State, error) {
	if err := a.match(c); err != nil {
		return connector.State{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return connector.State{Correlation: a.corr, Actual: a.actual, Active: a.active, Terminal: a.terminal, ThreadStatus: a.threadStatus, Settled: a.terminal != connector.TerminalNone && a.threadStatus == "idle"}, nil
}

// WaitSettled requires matching turn/completed evidence, then thread/read idle.
func (a *Adapter) WaitSettled(ctx context.Context, c connector.Correlation) (connector.SettlementReceipt, error) {
	if !a.caps.Quiescence {
		return connector.SettlementReceipt{}, unsupported("wait settled")
	}
	if err := a.match(c); err != nil {
		return connector.SettlementReceipt{}, err
	}
	if err := a.waitTerminal(ctx, ""); err != nil {
		return connector.SettlementReceipt{}, err
	}
	var rr struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := a.rpc.call(ctx, "thread/read", map[string]any{"threadId": c.NativeSessionID, "includeTurns": true}, &rr); err != nil {
		return connector.SettlementReceipt{}, wrap("wait settled", connector.ErrorTransport, err)
	}
	if rr.Thread.ID != c.NativeSessionID || rr.Thread.Status.Type != "idle" {
		return connector.SettlementReceipt{}, wrap("wait settled", connector.ErrorNotSettled, errors.New("terminal turn is not quiescent"))
	}
	now := a.clock.Now()
	a.mu.Lock()
	a.threadStatus = "idle"
	term, at, corr := a.terminal, a.terminalAt, a.corr
	a.mu.Unlock()
	return connector.SettlementReceipt{Correlation: corr, Terminal: term, ThreadStatus: "idle", TerminalObservedAt: at, QuiescenceObservedAt: now}, nil
}

// Steer adds text to the advertised active turn.
func (a *Adapter) Steer(ctx context.Context, r connector.SteerRequest) (connector.SteerReceipt, error) {
	if !a.caps.Steer {
		return connector.SteerReceipt{}, unsupported("steer")
	}
	if err := a.match(r.Correlation); err != nil {
		return connector.SteerReceipt{}, err
	}
	a.mu.Lock()
	active := a.active
	a.mu.Unlock()
	if !active {
		return connector.SteerReceipt{}, wrap("steer", connector.ErrorInvalid, errors.New("turn is not active"))
	}
	var out struct {
		TurnID string `json:"turnId"`
	}
	if err := a.rpc.call(ctx, "turn/steer", map[string]any{"threadId": r.Correlation.NativeSessionID, "expectedTurnId": r.Correlation.NativeTurnID, "input": []any{map[string]any{"type": "text", "text": r.Input}}}, &out); err != nil {
		return connector.SteerReceipt{}, wrap("steer", connector.ErrorTransport, err)
	}
	if out.TurnID != r.Correlation.NativeTurnID {
		return connector.SteerReceipt{}, wrap("steer", connector.ErrorProtocol, errors.New("turn id mismatch"))
	}
	return connector.SteerReceipt{Correlation: r.Correlation, Accepted: true}, nil
}

// Cancel interrupts the active turn and requires interrupted plus zero descendants.
func (a *Adapter) Cancel(ctx context.Context, r connector.CancelRequest) (connector.CancellationReceipt, error) {
	if !a.caps.Cancel {
		return connector.CancellationReceipt{}, unsupported("cancel")
	}
	if a.supervisor == nil {
		return connector.CancellationReceipt{}, unsupported("cancel supervisor proof")
	}
	if err := a.match(r.Correlation); err != nil {
		return connector.CancellationReceipt{}, err
	}
	if err := a.rpc.call(ctx, "turn/interrupt", map[string]any{"threadId": r.Correlation.NativeSessionID, "turnId": r.Correlation.NativeTurnID}, nil); err != nil {
		return connector.CancellationReceipt{}, wrap("cancel", connector.ErrorTransport, err)
	}
	if err := a.waitTerminal(ctx, "interrupted"); err != nil {
		return connector.CancellationReceipt{}, err
	}
	zero, err := a.supervisor.WaitZeroDescendants(ctx, r.Correlation)
	if err != nil {
		return connector.CancellationReceipt{}, wrap("cancel", connector.ErrorDescendantsRemain, err)
	}
	if !zero {
		return connector.CancellationReceipt{}, wrap("cancel", connector.ErrorDescendantsRemain, errors.New("zero-descendant proof refused"))
	}
	now := a.clock.Now()
	a.mu.Lock()
	at := a.terminalAt
	a.mu.Unlock()
	return connector.CancellationReceipt{Correlation: r.Correlation, Terminal: connector.TerminalInterrupted, ZeroDescendants: true, InterruptedAt: at, ProvedAt: now}, nil
}

// Collect delegates reference-only artifact collection.
func (a *Adapter) Collect(ctx context.Context, r connector.CollectRequest) (connector.ArtifactBundle, error) {
	if a.collector == nil {
		return connector.ArtifactBundle{}, unsupported("collect")
	}
	if err := a.match(r.Correlation); err != nil {
		return connector.ArtifactBundle{}, err
	}
	b, err := a.collector.Collect(ctx, r)
	if err != nil {
		return connector.ArtifactBundle{}, wrap("collect", connector.ErrorTransport, err)
	}
	for _, x := range b.References {
		if x.URI == "" || len(x.SHA256) != 64 || x.Size < 0 {
			return connector.ArtifactBundle{}, wrap("collect", connector.ErrorProtocol, errors.New("invalid artifact reference"))
		}
	}
	return b, nil
}

// Dispose closes native resources but never deletes workspace or evidence.
func (a *Adapter) Dispose(_ context.Context, r connector.DisposeRequest) error {
	if err := a.match(r.Correlation); err != nil {
		return err
	}
	a.mu.Lock()
	if a.disposed {
		a.mu.Unlock()
		return nil
	}
	a.disposed = true
	a.mu.Unlock()
	return a.rpc.close()
}

func (a *Adapter) notification(method string, p json.RawMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.corr.NativeTurnID == "" {
		switch method {
		case "turn/completed", "thread/status/changed", "model/rerouted":
			if len(a.deferred) >= cap(a.events) {
				return errors.New("deferred notification buffer exhausted")
			}
			a.deferred = append(a.deferred, deferredNotification{method: method, params: append(json.RawMessage(nil), p...)})
		}
		return nil
	}
	return a.notificationLocked(method, p)
}

func (a *Adapter) notificationLocked(method string, p json.RawMessage) error {
	switch method {
	case "turn/completed":
		var n struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(p, &n) != nil {
			return errors.New("malformed turn/completed notification")
		}
		if n.ThreadID != a.corr.NativeSessionID || n.Turn.ID != a.corr.NativeTurnID {
			return errors.New("wrong thread or turn in terminal notification")
		}
		switch n.Turn.Status {
		case "completed":
			a.terminal = connector.TerminalCompleted
		case "interrupted":
			a.terminal = connector.TerminalInterrupted
		case "failed":
			a.terminal = connector.TerminalFailed
		default:
			return errors.New("turn/completed carried nonterminal status")
		}
		a.active = false
		a.terminalAt = a.clock.Now()
		a.emitLocked(connector.EventTerminal, a.terminal, nil, "native terminal evidence")
		a.signalLocked()
	case "thread/status/changed":
		var n struct {
			ThreadID string `json:"threadId"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		if json.Unmarshal(p, &n) != nil || n.ThreadID != a.corr.NativeSessionID {
			return errors.New("wrong or malformed thread status notification")
		}
		a.threadStatus = n.Status.Type
		a.emitLocked(connector.EventNativeState, connector.TerminalNone, nil, n.Status.Type)
		a.signalLocked()
	case "model/rerouted":
		var n struct{ From, To, Reason, ThreadID, TurnID string }
		var wire struct {
			From     string `json:"fromModel"`
			To       string `json:"toModel"`
			Reason   string `json:"reason"`
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		if json.Unmarshal(p, &wire) != nil || wire.ThreadID != a.corr.NativeSessionID || wire.TurnID != a.corr.NativeTurnID {
			return errors.New("wrong or malformed model reroute notification")
		}
		n.From, n.To, n.Reason = wire.From, wire.To, wire.Reason
		if n.From != a.actual.Model {
			return errors.New("model reroute source mismatch")
		}
		a.actual.Model = n.To
		a.actual.EvidenceSource = "model/rerouted notification: " + n.Reason
		a.actual.ObservedAt = a.clock.Now()
		actual := a.actual
		a.emitLocked(connector.EventRerouted, connector.TerminalNone, &actual, "native model reroute")
	}
	return nil
}
func (a *Adapter) emitLocked(k connector.EventKind, s connector.TerminalStatus, id *connector.ActualIdentity, d string) {
	a.seq++
	e := connector.Event{Correlation: a.corr, Sequence: a.seq, At: a.clock.Now(), Kind: k, Status: s, Actual: id, Detail: d}
	select {
	case a.events <- e:
	default:
		a.rpc.fail(errors.New("connector event buffer exhausted"))
	}
}
func (a *Adapter) signalLocked() { close(a.changed); a.changed = make(chan struct{}) }
func (a *Adapter) waitTerminal(ctx context.Context, want string) error {
	for {
		a.mu.Lock()
		t, ch := a.terminal, a.changed
		a.mu.Unlock()
		if t != connector.TerminalNone {
			if want != "" && string(t) != want {
				return wrap("wait terminal", connector.ErrorProtocol, errors.New("unexpected terminal status"))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return wrap("wait terminal", connector.ErrorTimeout, ctx.Err())
		case <-ch:
		case <-a.rpc.done:
			return wrap("wait terminal", connector.ErrorTransport, io.EOF)
		}
	}
}
func (a *Adapter) match(c connector.Correlation) error {
	if err := validCorrelation(c, true); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !same(c, a.corr) {
		return wrap("correlation", connector.ErrorInvalid, errors.New("attempt correlation mismatch"))
	}
	return nil
}
func same(a, b connector.Correlation) bool {
	return a.RunID == b.RunID && a.Generation == b.Generation && a.AttemptKey == b.AttemptKey && a.AttemptID == b.AttemptID && a.RouteDigest == b.RouteDigest && a.WorkspaceLeaseID == b.WorkspaceLeaseID && a.WorkspaceLeaseEpoch == b.WorkspaceLeaseEpoch && a.ConnectorProfileID == b.ConnectorProfileID && a.ConnectorProfileDigest == b.ConnectorProfileDigest && a.Requested == b.Requested && a.NativeSessionID == b.NativeSessionID && a.NativeTurnID == b.NativeTurnID
}
func validCorrelation(c connector.Correlation, native bool) error {
	if c.RunID == "" || c.Generation < 1 || c.AttemptKey == "" || c.AttemptID == "" || c.RouteDigest == "" || c.WorkspaceLeaseID == "" || c.WorkspaceLeaseEpoch < 1 || c.ConnectorProfileID == "" || c.ConnectorProfileDigest == "" || c.Requested.Model == "" || c.Requested.Provider == "" || (native && (c.NativeSessionID == "" || c.NativeTurnID == "")) {
		return wrap("validate", connector.ErrorInvalid, errors.New("incomplete correlation"))
	}
	return nil
}
func renderBrief(b connector.ExecutionBrief) string {
	var s strings.Builder
	s.WriteString("Goal:\n")
	s.WriteString(b.Goal)
	sections := []struct {
		name string
		v    []string
	}{{"Constraints", b.Constraints}, {"Acceptance", b.Acceptance}, {"Proof", b.Proof}}
	if b.Rationale != "" {
		s.WriteString("\n\nRationale:\n")
		s.WriteString(b.Rationale)
	}
	for _, x := range sections {
		if len(x.v) > 0 {
			s.WriteString("\n\n" + x.name + ":\n")
			for _, v := range x.v {
				s.WriteString("- " + v + "\n")
			}
		}
	}
	return s.String()
}
func wrap(op string, k connector.ErrorKind, err error) error {
	if k == connector.ErrorTransport && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		k = connector.ErrorTimeout
	}
	return &connector.Error{Kind: k, Operation: op, Message: safeMessage(k), Err: err}
}
func safeMessage(k connector.ErrorKind) string { return string(k) }
func unsupported(op string) error {
	return wrap(op, connector.ErrorUnsupported, errors.New("capability unavailable"))
}

var _ connector.Connector = (*Adapter)(nil)
