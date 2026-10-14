package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// SCHED-GAP-119: activity-aware supervision of the gateway /v1/responses POST.
//
// SCHED-GAP-117 armed a pure WALL-CLOCK deadline (default 30m) around the
// POST. On its first live day that deadline failed 15 ticks across 13
// projects with "stalled: no progress" — including a hermes-dagger tick
// whose window contains real commits (e87d709 at 16:38, board close c27d5c3
// at 16:49, verified via git log) while the scheduler recorded a
// zero-accounted failure. The non-streaming POST is invisible: nothing
// arrives on the wire until the whole multi-hour turn completes, so
// wall-clock idleness and server-side productivity are indistinguishable.
//
// The fix keeps the same knob but moves to the gateway's SSE surface: the
// POST now carries stream:true and the deadline becomes an IDLE deadline —
// reset by every real SSE event (response.created, output_text.delta,
// output_item.added/done, response.completed/failed). SSE comments
// (`: keepalive`, emitted by the gateway every 30s REGARDLESS of agent
// liveness — source-verified in gateway/platforms/api_server_openai_routes.py
// _iter_stream_items) are deliberately NOT activity. A genuinely hung POST
// (no bytes at all — the GAP-117 signature) never resets the timer, so it
// aborts at the same deadline as before.
//
// Fail-open (AC 3): a server that answers non-SSE (JSON — an older gateway
// or a route that ignores stream:true) provides no activity signal; the
// timer then never resets and behaves EXACTLY like the GAP-117 wall clock.
// With the knob disabled (0) or collapsed onto the tick deadline
// (>= effectiveTimeout) spawn.go keeps the legacy non-streaming code path
// byte-for-byte.

// TurnDeadlineError is returned when the per-turn idle/wall deadline fired
// and the POST was aborted locally (SCHED-GAP-119). The tick deadline was
// still alive when it tripped — spawn.go classifies the tick as the
// GAP-117 "stalled: no progress" failure off this sentinel. It is
// deliberately NOT transient (IsTransientGatewayErr): re-sending a POST
// that just went idle for the whole deadline would double-run the foreman.
type TurnDeadlineError struct {
	// After is the configured per-turn deadline that fired.
	After time.Duration
	// Events is the number of real SSE events observed before the abort
	// (0 = the POST never became observable — the classic idle-POST hang).
	Events int
}

// Error implements error. The GAP-117 "stalled" contract text itself is
// built in spawn.go; this text only names the mechanism for logs.
func (e *TurnDeadlineError) Error() string {
	return fmt.Sprintf("gateway turn deadline exceeded: no activity for %v (%d events observed)", e.After, e.Events)
}

// GatewayPOSTTrace is the per-POST record required by SCHED-GAP-119 AC 1:
// start/finish timestamps, elapsed, the deadline applied, and a
// classification — logged as a stable grep-able scheduler.log line
// ("GATEWAY-POST-TRACE:") and persisted on the tick row
// (ticks.gateway_trace JSON).
type GatewayPOSTTrace struct {
	// TickID and Project are stamped by spawn.go before persistence.
	TickID  string `json:"tick_id"`
	Project string `json:"project"`
	// Model and Provider name the pair the trace covers (the LAST attempt
	// when the retry/chain-hop loop re-sent).
	Model    string `json:"model"`
	Provider string `json:"provider"`
	// Start/Finish bracket the supervised POST window (first attempt start
	// to last attempt finish). Elapsed is Finish-Start.
	Start   time.Time     `json:"start"`
	Finish  time.Time     `json:"finish"`
	Elapsed time.Duration `json:"-"`
	// ElapsedMS is Elapsed in milliseconds (time.Duration marshals as bare
	// nanoseconds; the _ms field keeps the persisted JSON readable).
	ElapsedMS int64 `json:"elapsed_ms"`
	// Deadline is the configured per-turn deadline; DeadlineMode is "idle"
	// (SSE activity resets the timer) or "wall" (no activity signal —
	// JSON fallback / legacy semantics).
	Deadline   time.Duration `json:"-"`
	DeadlineMS int64         `json:"deadline_ms"`
	// DeadlineMode: "idle" or "wall" (see Deadline).
	DeadlineMode string `json:"deadline_mode"`
	// Classification: completed | aborted-by-turn-deadline | transport-error.
	Classification string `json:"classification"`
	// IdleFired is true when the abort came from the resettable deadline
	// (as opposed to the parent session context or a transport failure).
	IdleFired bool `json:"idle_fired"`
	// Events counts real SSE events observed across attempts (deltas, tool
	// items, terminal events). Keepalive comments are excluded.
	Events int `json:"events"`
	// SessionID is the gateway session id from the X-Hermes-Session-Id
	// response header when present (available at stream open — earlier and
	// more precise than the per-request resp_* envelope id), else the
	// envelope id.
	SessionID string `json:"session_id"`
	// Attempts counts the POSTs covered by this trace (the GAP-080 retry
	// loop and chain hops each re-send under the same trace).
	Attempts int `json:"attempts"`
	// SCHED-GAP-1641: the OUTPUT side of the turn's usage, folded from the
	// terminal response envelope. TokensOut == 0 means ABSENT (the legacy
	// minimal traces never set it) — the instant-turn classifier keys on
	// that zero and fails open, so pre-1641 traces keep accounting healthy.
	TokensOut int `json:"tokens_out,omitempty"`
	// TokensIn is the PROMPT side of the usage. Same absent-means-zero
	// fail-open contract as TokensOut.
	TokensIn int `json:"tokens_in,omitempty"`
}

// SCHED-GAP-1641 named thresholds — the ONE-SHOT instant-turn fingerprint.
// Evidence (2026-09-25 burst): 241 ticks in one hour served single turns of
// 62-181 output tokens against a byte-identical 44,469-token prompt, all
// recorded committed. The floors deliberately sit ABOVE the burst's observed
// maxima (10 events, 300 tokens_out) and two orders of magnitude BELOW a
// productive tick (hundreds of events, thousands of output tokens), so a
// normal short reply never trips the band.
const (
	// instantTurnMaxEvents: SSE events observed for the whole turn (deltas
	// + tool items + terminal events; keepalives excluded). At or below 10.
	instantTurnMaxEvents = 10
	// instantTurnMaxTokensOut: output tokens billed for the turn. At or
	// below 300.
	instantTurnMaxTokensOut = 300
	// GatewayInstantTurnSentinel is the error-column marker carried by a
	// tick classified instant-turn. Listed in the HarnessFailure marker set
	// (failureclass.go) so failureReasonClass stamps
	// failure_reason=instant_turn and NO healthy-tick surface counts it.
	GatewayInstantTurnSentinel = "instant one-shot gateway turn"
)

// SCHED-GAP-1655 zero-tool detection.
//
// responseHasToolCall reports whether a terminal response envelope carries
// ANY tool-call output item — the OpenAI Responses wire shapes the gateway
// streams back ("function_call", "tool_use", "custom_tool_call", and the
// local shell/apply variants). A completed response with output text but no
// tool item means the agent talked and never acted.
//
// Unknown item types (a future wire shape) are NOT counted as tools: the
// classifier must under-count (leave a acted-on tick honestly classified
// by its artifacts) rather than over-count (hide a real zero-tool tick
// behind an unrecognized type name).
func responseHasToolCall(resp *Response) bool {
	if resp == nil {
		return false
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call", "tool_use", "custom_tool_call",
			"local_shell_call", "apply_patch_call":
			return true
		}
	}
	return false
}

// zeroToolSession reports whether a completed gateway tick's session ran
// ZERO tool calls (SCHED-GAP-1655 deliverable 2) — the terminal statement
// that the lane had nothing to do. Two arms, both required to stay silent:
//
//   - the response envelope carries no tool-call item (responseHasToolCall
//     false), AND
//   - the SSE trace observed fewer than 2 real events. The trace is the
//     belt to the envelope's braces: a mid-stream tool item arrives as its
//     own SSE event before the terminal envelope, so a turn that acted
//     always shows >= 2 events (the tool event + the terminal event) even
//     if the terminal envelope's output were truncated by a gateway bug.
//     A trace with Events == 0 observed nothing (pre-119 legacy, or the
//     JSON fallback) — the envelope alone decides there.
//
// TokensIn is deliberately unconstrained: the fleet's zero-tool ticks are
// PROMPTED sessions (44k-token foreman prompt in, tiny reply out) — billing
// input is what makes them waste, not evidence of work.
func zeroToolSession(resp *Response, trace *GatewayPOSTTrace) bool {
	if responseHasToolCall(resp) {
		return false
	}
	if trace != nil && trace.Events >= 2 {
		return false
	}
	return true
}

// isInstantOneShotTurn (SCHED-GAP-1641) reports whether a merged tick trace
// carries the one-shot fingerprint: the usage fields are PRESENT (non-zero —
// zero means the legacy trace never recorded them) AND both output-side
// floors hold. The INPUT side is deliberately unconstrained: a huge prompt
// with a tiny reply is exactly the burst shape. A trace with no usage fields
// stays healthy — fail-open for legacy traces.
func isInstantOneShotTurn(trace *GatewayPOSTTrace) bool {
	if trace == nil {
		return false
	}
	if trace.TokensOut == 0 && trace.TokensIn == 0 {
		return false
	}
	if trace.Events <= instantTurnMaxEvents && trace.TokensOut <= instantTurnMaxTokensOut {
		return true
	}
	return false
}

// mergePostTrace folds one attempt's trace into the accumulating tick trace
// (SCHED-GAP-119 AC 1: one trace per tick covering every POST). The final
// attempt's classification/deadline-mode wins; events accumulate; the
// earliest start and latest finish bracket the window.
func mergePostTrace(dst **GatewayPOSTTrace, next *GatewayPOSTTrace) {
	if next == nil {
		return
	}
	if *dst == nil {
		clone := *next
		*dst = &clone
		return
	}
	d := *dst
	d.Attempts++
	d.Finish = next.Finish
	d.Elapsed = d.Finish.Sub(d.Start)
	d.ElapsedMS = d.Elapsed.Milliseconds()
	d.Deadline = next.Deadline
	d.DeadlineMS = next.DeadlineMS
	d.DeadlineMode = next.DeadlineMode
	d.Classification = next.Classification
	d.IdleFired = next.IdleFired
	d.Events += next.Events
	if next.SessionID != "" {
		d.SessionID = next.SessionID
	}
}

// turnWatch supervises one POST with a resettable idle deadline. The
// watcher goroutine owns a child context; when the deadline fires (no
// activity for `deadline`) it marks fired and cancels the context, which
// aborts the in-flight POST. Every real SSE event resets the timer.
type turnWatch struct {
	deadline time.Duration
	fired    atomic.Bool
	events   atomic.Int64
	activity chan struct{}
	stopped  chan struct{}
	cancel   context.CancelFunc
}

// startTurnWatch arms the idle deadline for one POST. The returned context
// is the request context: canceled by the watcher when the deadline fires,
// or by Stop when the POST resolves. deadline must be > 0.
func startTurnWatch(parent context.Context, deadline time.Duration) (*turnWatch, context.Context) {
	w := &turnWatch{
		deadline: deadline,
		activity: make(chan struct{}, 1),
		stopped:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel = cancel
	go func() {
		defer close(w.stopped)
		t := time.NewTimer(deadline)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				w.fired.Store(true)
				cancel()
				return
			case <-w.activity:
				// Sole resetter: the only exits from this select are the
				// timer firing or ctx cancellation, so Reset is race-free
				// per the time package docs.
				t.Reset(deadline)
			case <-ctx.Done():
				return
			}
		}
	}()
	return w, ctx
}

// notify records one real SSE event and pokes the watcher so the idle
// timer resets. Non-blocking: a dropped poke only means the timer resets on
// the next event.
func (w *turnWatch) notify() {
	w.events.Add(1)
	select {
	case w.activity <- struct{}{}:
	default:
	}
}

// stop cancels the request context (no-op if already canceled) and waits
// for the watcher goroutine to exit, freezing the fired/events state.
func (w *turnWatch) stop() {
	w.cancel()
	<-w.stopped
}

// abortErr maps a POST/transport error to the SCHED-GAP-119
// classifications: when the watch fired, the error is a TurnDeadlineError
// (idle abort while the tick deadline was still alive); otherwise it is the
// legacy transport error, unchanged.
func (w *turnWatch) abortErr(err error) error {
	if w.fired.Load() {
		return &TurnDeadlineError{After: w.deadline, Events: int(w.events.Load())}
	}
	return err
}

// SendResponseStream sends the prompt to /v1/responses with stream:true and
// supervises the turn with a resettable per-turn deadline (SCHED-GAP-119).
//
// ctx must be the SESSION context (tick deadline): it bounds the absolute
// maximum wall time. turnDeadline is the per-turn knob; while > 0 a timer
// of that duration starts at POST launch and is reset by every real SSE
// event, so only NO-ACTIVITY for turnDeadline aborts the POST with a
// *TurnDeadlineError. A server answering non-SSE provides no resets and
// gets exactly the GAP-117 wall-clock behavior (fail-open, AC 3).
//
// The returned Response is assembled from the response.completed envelope —
// the same shape the non-streaming body carries (id/status/output/usage) —
// so the spawn path's completion gate is unchanged. response.failed maps to
// the same error-envelope handling as the legacy path. The trace carries
// the AC 1 record; its Classification is finalized on every return path.
func (g *GatewayClient) SendResponseStream(ctx context.Context, prompt, model, provider, key, sessionKey string, turnDeadline time.Duration) (*Response, *GatewayPOSTTrace, error) {
	trace := &GatewayPOSTTrace{
		Start:          g.clock().Now(),
		Deadline:       turnDeadline,
		DeadlineMode:   "wall",
		Model:          model,
		Provider:       provider,
		Classification: "transport-error",
		Attempts:       1,
	}
	defer func() {
		trace.Finish = g.clock().Now()
		trace.Elapsed = trace.Finish.Sub(trace.Start)
		trace.ElapsedMS = trace.Elapsed.Milliseconds()
		trace.DeadlineMS = trace.Deadline.Milliseconds()
	}()

	noApproval := false
	stream := true
	reqBody := ResponseRequest{
		Input:           prompt,
		Model:           model,
		Provider:        provider,
		RequireApproval: &noApproval,
		Stream:          &stream, // SCHED-GAP-119: make turn activity observable
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, trace, fmt.Errorf("marshal request: %w", err)
	}

	// The idle watch cancels this child context on the deadline; the
	// parent (session) context still bounds the absolute wall time.
	var watch *turnWatch
	reqCtx := ctx
	if turnDeadline > 0 {
		watch, reqCtx = startTurnWatch(ctx, turnDeadline)
		defer watch.stop()
	}

	req, err := http.NewRequestWithContext(reqCtx, "POST", g.baseURL+"/v1/responses",
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, trace, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sessionKey != "" {
		req.Header.Set("X-Hermes-Session-Key", sessionKey)
	}
	g.setAuth(req, key)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		if watch != nil {
			return nil, trace, watch.abortErr(fmt.Errorf("gateway POST: %w", err))
		}
		return nil, trace, fmt.Errorf("gateway POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The real gateway session id rides the response header and is present
	// at stream open — earlier and more precise than the per-request resp_*
	// envelope id (S-GAP-003/GAP-074 linkage).
	if sid := resp.Header.Get("X-Hermes-Session-Id"); sid != "" {
		trace.SessionID = sid
	}

	// GAP-035 parity: the response STATUS is authoritative; classifications
	// are byte-identical to the legacy path.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, trace, fmt.Errorf("%w (HTTP %d): %s", ErrGatewayKeyRejected, resp.StatusCode, authErrorDetail(body))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// SCHED-GAP-080 parity: carry the status code so the spawn path
		// can classify 5xx as transient; Error() text is byte-identical.
		return nil, trace, &GatewayStatusError{
			StatusCode: resp.StatusCode,
			msg:        fmt.Sprintf("gateway POST: HTTP %d: %s", resp.StatusCode, authErrorDetail(body)),
		}
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		trace.DeadlineMode = "idle"
		r, err := readSSEResponse(resp.Body, watch)
		if watch != nil {
			trace.Events = int(watch.events.Load())
			if err != nil {
				if werr := watch.abortErr(err); werr != err {
					// The watch fired — this is the idle abort.
					trace.Classification = "aborted-by-turn-deadline"
					trace.IdleFired = true
					return nil, trace, werr
				}
			}
		}
		if err != nil {
			return nil, trace, err
		}
		if trace.SessionID == "" && r.ID != "" {
			trace.SessionID = r.ID
		}
		// SCHED-GAP-1641: fold the terminal envelope's usage into the trace
		// so the instant-turn classifier can see the output side.
		trace.TokensIn, trace.TokensOut = r.Usage.InputTokens, r.Usage.OutputTokens
		trace.Classification = "completed"
		return r, trace, nil
	}

	// JSON fallback (AC 3): the server ignored stream:true (older gateway,
	// test double, route change). No activity signal exists, so the —
	// still un-reset — deadline timer IS the GAP-117 wall clock: identical
	// behavior to today on that path, no new blind burn.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if watch != nil {
			return nil, trace, watch.abortErr(fmt.Errorf("read response: %w: %w", ErrGatewayTransient, err))
		}
		return nil, trace, fmt.Errorf("read response: %w: %w", ErrGatewayTransient, err)
	}
	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		if watch != nil {
			return nil, trace, watch.abortErr(fmt.Errorf("unmarshal response: %w: %w", ErrGatewayTransient, err))
		}
		return nil, trace, fmt.Errorf("unmarshal response: %w: %w", ErrGatewayTransient, err)
	}
	if result.Error != nil {
		return nil, trace, fmt.Errorf("gateway error: %s — %s", result.Error.Type, result.Error.Message)
	}
	if watch != nil {
		trace.Events = int(watch.events.Load())
	}
	if trace.SessionID == "" && result.ID != "" {
		trace.SessionID = result.ID
	}
	trace.TokensIn, trace.TokensOut = result.Usage.InputTokens, result.Usage.OutputTokens
	trace.Classification = "completed"
	return &result, trace, nil
}

// readSSEResponse consumes the SSE stream, notifying the idle watch on
// every real event, and returns the Response assembled from the terminal
// envelope (response.completed / response.failed — identical shape to the
// non-streaming body). A stream that ends without a terminal event is a
// transient (the gateway died mid-turn).
func readSSEResponse(body io.Reader, watch *turnWatch) (*Response, error) {
	reader := bufio.NewReader(body)
	var eventName string
	dataLines := make([]string, 0, 4)
	notify := func() {
		if watch != nil {
			watch.notify()
		}
	}
	handleEvent := func() (*Response, bool, error) {
		if len(dataLines) == 0 {
			return nil, false, nil
		}
		data := strings.Join(dataLines, "\n")
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &probe); err != nil {
			// Non-JSON data line — something was written; count it as
			// activity (fail-open favors the turn) and keep reading.
			notify()
			return nil, false, nil
		}
		evt := probe.Type
		if evt == "" {
			evt = eventName
		}
		switch evt {
		case "response.completed", "response.failed":
			notify()
			var env struct {
				Response Response       `json:"response"`
				Error    *ResponseError `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &env); err != nil {
				return nil, true, fmt.Errorf("unmarshal %s envelope: %w: %w", evt, ErrGatewayTransient, err)
			}
			r := env.Response
			if r.Status == "" {
				r.Status = "completed"
			}
			if evt == "response.failed" {
				msg := "agent turn failed"
				switch {
				case env.Error != nil && env.Error.Message != "":
					msg = env.Error.Message
				case r.Error != nil && r.Error.Message != "":
					msg = r.Error.Message
				}
				r.Error = &ResponseError{Type: "agent_error", Message: msg}
			}
			return &r, true, nil
		default:
			// Any other typed event (response.created, output_text.delta,
			// output_item.added/done, ...) is real activity.
			notify()
			return nil, false, nil
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// A buffered terminal event may still be pending.
				if resp, done, herr := handleEvent(); done {
					return resp, herr
				}
				if watch != nil {
					if werr := watch.abortErr(nil); werr != nil {
						return nil, werr
					}
				}
				if errors.Is(err, context.DeadlineExceeded) {
					// SCHED-GAP-1684: the READ ended because the SESSION
					// context hit its deadline — the tick's own wall (the
					// per-turn abort is the TurnDeadlineError above). WE tore
					// the stream down; the gateway did not. Wrap the deadline
					// itself so the spawn path can book a self-timeout
					// instead of charging our wall to the gateway as a
					// transient blip (30 of 41 deferred ticks in the
					// 2026-09-30 sample died at exactly 120/180 min).
					return nil, fmt.Errorf("%w: session context deadline expired during sse stream", ErrTickDeadlineExceeded)
				}
				return nil, fmt.Errorf("%w: sse stream ended without a terminal event", ErrGatewayTransient)
			}
			if watch != nil {
				if werr := watch.abortErr(nil); werr != nil {
					return nil, werr
				}
			}
			return nil, fmt.Errorf("read sse: %w: %w", ErrGatewayTransient, err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// Blank line = end of event block.
			if resp, done, herr := handleEvent(); done {
				return resp, herr
			}
			eventName = ""
			dataLines = dataLines[:0]
		case strings.HasPrefix(line, ":"):
			// SSE comment — the gateway's 30s keepalive. Deliberately NOT
			// activity: it is emitted on a bare timer regardless of agent
			// liveness (source-verified in _iter_stream_items).
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}
