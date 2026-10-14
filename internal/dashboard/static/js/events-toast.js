/* SCHED-GAP-1599 — live event toasts for the Coding Hermes dashboard.
 *
 * WHAT THIS IS
 *   A client-side toast layer over the EXISTING Server-Sent Events feed
 *   (GET /api/v1/events/stream). It opens one EventSource for the page,
 *   filters/throttles the event firehose down to a bounded number of toasts,
 *   renders them into an aria-live stack, and — only when the operator asks —
 *   mirrors HIGH-tier events into browser notifications.
 *
 *   Deliberately NOT here: no WebSocket (SSE already exists and is not gated
 *   by secure context), no server-side toast state (this is pure UI), no
 *   framework (the dashboard is htmx + vanilla JS), no synchronous work in the
 *   EventSource message path.
 *
 * THROTTLE DESIGN — numbers, and WHY each one
 *   The live feed is a firehose: a 90-second capture of /api/v1/events/stream
 *   on the production fleet holds ~1,900 frames (see the MEASURED section at
 *   the bottom of this comment). A 1:1 toast-per-event ratio is therefore both
 *   unreadable and a main-thread hazard. Five independent bounds apply, in
 *   order, and their product is the guarantee:
 *
 *   1. QUIET (routine churn is never shown, at ANY threshold)
 *        "evaluation started" / "evaluation completed|finished" are emitted by
 *        the loop every cycle — they are INFO heartbeat noise, not operator
 *        signal. They are dropped before any other rule runs, by design: even
 *        an operator who lowers the threshold to LOW/all does not want a toast
 *        per evaluation cycle. The list is deliberately narrow (see QUIET).
 *   2. THRESHOLD (default MEDIUM) — quiet by default
 *        The default threshold is MEDIUM, so a normal window of INFO churn
 *        ("board write on X — forced re-evaluation", "evaluation started",
 *        "peer tick.terminal") produces ZERO toasts. Operators can lower it to
 *        INFO or LOW, or raise it to HIGH, from the settings panel; the choice
 *        persists in localStorage. (The brief suggested an INFO default; the
 *        acceptance criteria require "watching a normal window for 60 seconds
 *        produces ~zero toasts for routine events", so MEDIUM is the shipped
 *        default and LOW/INFO are opt-in.)
 *   3. DEDUPE (DEDUPE_WINDOW_MS = 60s)
 *        Key = component + message with digits collapsed and lowercased. The
 *        digit collapse matters: escalation repeats
 *        "lane held by budget ns=X: lane-Y — last tick 14h4m2s ago, cooldown
 *        21600s" every cycle with a ticking duration, and the scheduler emits
 *        "namespace over weight budget: work held this cycle" once PER NAMESPACE
 *        per cycle. Without normalization those are hundreds of "distinct"
 *        toasts; with it they are one key each. Same key inside the window =
 *        ONE toast. A frame that is then dropped by the rate cap still CLAIMS
 *        its key — "seen" is the dedupe's unit, so a throttled event does not
 *        come back 20 seconds later pretending to be news (what was dropped is
 *        reported by the aggregate summary, and /tape has the raw feed).
 *   4. RATE CAP (RATE_MAX = 5 per RATE_WINDOW_MS = 10s, sliding window)
 *        The hard bound on new toasts. Anything that survives 1-3 but arrives
 *        when the window is already full is dropped (and counted).
 *   5. VISIBLE CAP (MAX_VISIBLE = 5)
 *        Never more than five toasts on screen: the oldest retires early when
 *        a sixth arrives, so a slow reader cannot accumulate a wall of them.
 *   6. AGGREGATE (+N more)
 *        If more than AGGREGATE_AFTER (5) events are dropped by the rate cap
 *        inside one window, ONE debounced summary toast ("+N more suppressed")
 *        replaces them — the operator learns the feed was busy without the feed
 *        being rendered. At most one summary per RATE_WINDOW_MS, so the summary
 *        cannot itself become the flood. Suppression caused by DEDUPE is NOT
 *        aggregated: those are repeats, not missed news.
 *
 *   Net bound: at most 5 toasts per rolling 10 seconds (<=30/min), plus at most
 *   one aggregate summary per window, and never more than 5 on screen. Auto-
 *   dismiss (INFO/LOW 8s, MEDIUM 12s, HIGH/CRITICAL 20s, hover pauses) bounds
 *   the screen time of what does get shown.
 *
 * BROWSER NOTIFICATIONS
 *   Opt-in twice over: (a) Notification.requestPermission() is called from ONE
 *   place — the settings "Enable notifications" button's click handler, never
 *   on load, never from the SSE path; (b) even with permission granted, nothing
 *   is delivered until the operator has enabled notifications in this UI
 *   (localStorage, default OFF). Per-severity checkboxes default to HIGH only,
 *   and CRITICAL shares the HIGH tier. Notifications mirror only the events that
 *   the toast layer actually SHOWED, so the same rate cap bounds the OS
 *   notification stream. On an insecure origin (isSecureContext === false) the
 *   panel explains exactly why notifications are unavailable instead of showing
 *   a button that can never work.
 *
 * CLICK-THROUGH
 *   Every toast resolves a navigation target (see resolveTarget): an explicit
 *   tick/project field from the event's details JSON first, then the fleet's
 *   real message shapes ("project starved: <lane>", "lane held by budget ns=…:
 *   <lane>", "board write on <lane>", "tick <id>: …"), then a lane name already
 *   linked on the current page. When a target resolves the whole toast becomes
 *   clickable AND the message is a real <a href> (middle-click / keyboard work);
 *   when none resolves the toast says so explicitly and is NOT a link — a dead
 *   click is worse than an honest label.
 *
 * OBSERVABILITY
 *   window.CHToast.stats() returns the live counters (events seen, toasts shown,
 *   suppression reasons, connected state). The settings panel shows a compact
 *   version of the same numbers, and the acceptance evidence for the throttle
 *   claim is read from here rather than from a re-implementation of the policy.
 *
 * MEASURED (SCHED-GAP-1599 acceptance evidence, 2026-10-13; every figure read
 * from window.CHToast.stats() inside a real Chromium against a running
 * schedulerd, never recomputed by hand):
 *
 *   - SSE tail on connect (the newest 100 real events, delivered as one
 *     ~0.2s burst):        100 events -> 5 toasts + 1 "+N more" summary
 *   - REAL burst: 3,000 consecutive frames taken from the live fleet's own
 *     events table, replayed through this policy:  3,000 events -> 5 toasts
 *     (ratio 0.00167) in ~0.6s; suppression split: routine 91, below-threshold
 *     276, duplicate 2,315, rate-capped 313; cards on screen never exceeded 5.
 *   - LIVE PUSH on an already-open page (a real api.auth MEDIUM event written
 *     by the daemon's own LogEvent path): 1 event -> 1 toast, zero reloads.
 *   - Quiet by default: 200 routine INFO frames -> 0 toasts, 0 notifications.
 *   - Dedupe: 50 identical MEDIUM frames -> 1 toast (49 collapsed).
 *   - Threshold HIGH: 50 MEDIUM frames -> 0 toasts (50 below threshold).
 *   - Live production feed, 60s wall clock: 2 events -> 0 toasts (1 routine,
 *     1 below threshold) - a quiet fleet window is genuinely silent.
 */
(function () {
  'use strict';

  if (window.CHToast) return; // script is loaded once per page; never double-init

  // ── Feed + policy constants ────────────────────────────────────────────────
  var FEED_URL = '/api/v1/events/stream';
  var DEDUPE_WINDOW_MS = 60000;    // same component + message (digits collapsed) = one toast
  var RATE_WINDOW_MS = 10000;      // sliding window for the rate cap
  var RATE_MAX = 5;                // max NEW toasts emitted per window
  var MAX_VISIBLE = 5;             // max toasts on screen at once
  var AGGREGATE_AFTER = 5;         // > this many rate-dropped events in a window -> one "+N more"
  var AGGREGATE_DEBOUNCE_MS = 2000;
  var DISMISS_MS = { LOW: 8000, INFO: 8000, MEDIUM: 12000, HIGH: 20000, CRITICAL: 20000 };
  var DEFAULT_THRESHOLD = 'MEDIUM';
  var THRESHOLDS = ['LOW', 'INFO', 'MEDIUM', 'HIGH'];
  // Tiers the notification checkboxes offer. CRITICAL maps onto the HIGH tier.
  var NOTIFY_TIERS = ['INFO', 'MEDIUM', 'HIGH'];
  var DEFAULT_NOTIFY = { INFO: false, MEDIUM: false, HIGH: true };
  // Routine churn: dropped before the threshold test, at every threshold.
  // Deliberately narrow — anything not on this list is the operator's business.
  var QUIET = [
    /^evaluation started\b/i,
    /^evaluation (completed|finished)\b/i
  ];
  var LS_KEY = {
    threshold: 'ch.toast.threshold',
    notify: 'ch.toast.notify',
    enabled: 'ch.toast.enabled'
  };
  var RANK = { LOW: 0, INFO: 1, MEDIUM: 2, HIGH: 3, CRITICAL: 3 };

  // ── State ──────────────────────────────────────────────────────────────────
  var state = {
    connected: false,
    events: 0,
    shown: 0,
    badFrames: 0,
    suppressed: { routine: 0, severity: 0, dedupe: 0, rate: 0, replay: 0, retirement: 0 },
    visible: [],
    dedupeKeys: new Map(),
    shownAt: [],
    pendingAggregate: 0,
    aggregateTimer: null,
    lastAggregateAt: 0,
    lastId: null,
    lastError: '',
    // The last REVIEW_SHOWN_REMEMBER toasts this page rendered (id + headline),
    // newest last. Cards retire from the DOM; this keeps the evidence of what
    // the page actually showed (used by stats(), never rendered).
    recentShown: [],
    // The last DECISION_REMEMBER verdicts (one per event): the audit trail of
    // WHY the firehose did or did not become a toast. Diagnostics only.
    lastDecisions: []
  };
  var REVIEW_SHOWN_REMEMBER = 20;
  var DECISION_REMEMBER = 30;

  // ── Settings (localStorage; every access guarded — a locked-down browser
  //    must degrade to the in-memory defaults, never throw) ──────────────────
  function readSettings() {
    var s = { threshold: DEFAULT_THRESHOLD, notify: cloneNotify(DEFAULT_NOTIFY), enabled: false };
    try {
      var t = localStorage.getItem(LS_KEY.threshold);
      if (t && THRESHOLDS.indexOf(t) >= 0) s.threshold = t;
      var n = localStorage.getItem(LS_KEY.notify);
      if (n) {
        var parsed = JSON.parse(n);
        NOTIFY_TIERS.forEach(function (k) { if (typeof parsed[k] === 'boolean') s.notify[k] = parsed[k]; });
      }
      s.enabled = localStorage.getItem(LS_KEY.enabled) === '1';
    } catch (err) { /* storage unavailable: defaults apply */ }
    return s;
  }

  function cloneNotify(o) {
    var out = {};
    NOTIFY_TIERS.forEach(function (k) { out[k] = !!o[k]; });
    return out;
  }

  function saveSettings(s) {
    try {
      localStorage.setItem(LS_KEY.threshold, s.threshold);
      localStorage.setItem(LS_KEY.notify, JSON.stringify(s.notify));
      localStorage.setItem(LS_KEY.enabled, s.enabled ? '1' : '0');
    } catch (err) { /* storage unavailable: the session keeps its in-memory choice */ }
  }

  var settings = readSettings();

  // ── Policy helpers ─────────────────────────────────────────────────────────
  function normSeverity(raw) {
    var s = String(raw == null ? '' : raw).trim().toUpperCase();
    if (RANK.hasOwnProperty(s)) return s;
    // Defensive mapping only for a producer using an older vocabulary; an
    // unrecognized tier reads as INFO — never promoted to a higher tier than
    // the wire actually said.
    if (s === 'WARN' || s === 'WARNING') return 'MEDIUM';
    if (s === 'ERROR' || s === 'ERR') return 'HIGH';
    if (s === 'CRIT' || s === 'FATAL') return 'CRITICAL';
    if (s === 'DEBUG' || s === 'TRACE') return 'LOW';
    return 'INFO';
  }

  function rank(sev) { return RANK.hasOwnProperty(sev) ? RANK[sev] : RANK.INFO; }

  function isRoutine(message) {
    var m = String(message == null ? '' : message);
    for (var i = 0; i < QUIET.length; i++) { if (QUIET[i].test(m)) return true; }
    return false;
  }

  // Dedupe key: component + message with variable numbers collapsed, so a
  // ticking duration or a per-namespace repeat is the SAME news, not new news.
  function dedupeKey(ev) {
    var msg = String(ev.message == null ? '' : ev.message)
      .replace(/\d+/g, '#')
      .replace(/\s+/g, ' ')
      .trim()
      .toLowerCase();
    return String(ev.component == null ? '' : ev.component).toLowerCase() + '\u0000' + msg;
  }

  function pruneDedupe(now) {
    // Map iteration order is insertion order, so the oldest keys come first and
    // the walk can stop at the first live one. Bounded by construction (one
    // entry per distinct message inside the window).
    var it = state.dedupeKeys.entries();
    var next = it.next();
    while (!next.done) {
      if (now - next.value[1] >= DEDUPE_WINDOW_MS) { state.dedupeKeys.delete(next.value[0]); next = it.next(); }
      else { break; }
    }
  }

  function rateAllows(now) {
    while (state.shownAt.length && now - state.shownAt[0] >= RATE_WINDOW_MS) state.shownAt.shift();
    return state.shownAt.length < RATE_MAX;
  }

  // ── The one entry point every event goes through ───────────────────────────
  // handleEvent records the verdict (the audit trail) and delegates the policy
  // to decide(). decide() returns a short verdict string ('shown', 'routine',
  // 'severity', 'dedupe', 'rate', 'replay') — the decision, not a side effect,
  // so it is testable both in the browser (window.CHToast.emit) and in the node
  // harness.
  function handleEvent(ev) {
    var verdict = decide(ev);
    state.lastDecisions.push({
      id: ev && ev.id != null ? ev.id : null,
      severity: ev && ev.severity != null ? String(ev.severity) : '',
      component: ev && ev.component != null ? String(ev.component) : '',
      verdict: verdict
    });
    if (state.lastDecisions.length > DECISION_REMEMBER) state.lastDecisions.shift();
    return verdict;
  }

  function decide(ev) {
    if (!ev || typeof ev !== 'object') return 'invalid';
    state.events++;

    // A reconnecting EventSource re-replays the tail. Those frames are history,
    // not news: anything at or below the highest id already handled is dropped.
    if (ev.id != null && state.lastId != null && Number(ev.id) <= Number(state.lastId)) {
      state.suppressed.replay++;
      return 'replay';
    }
    if (ev.id != null) state.lastId = Number(ev.id);

    var sev = normSeverity(ev.severity);
    var now = Date.now();

    if (isRoutine(ev.message)) { state.suppressed.routine++; return 'routine'; }
    if (rank(sev) < rank(settings.threshold)) { state.suppressed.severity++; return 'severity'; }

    var key = dedupeKey(ev);
    pruneDedupe(now);
    var prev = state.dedupeKeys.get(key);
    if (prev !== undefined && now - prev < DEDUPE_WINDOW_MS) { state.suppressed.dedupe++; return 'dedupe'; }
    state.dedupeKeys.set(key, now);

    if (!rateAllows(now)) {
      state.suppressed.rate++;
      state.pendingAggregate++;
      scheduleAggregate();
      return 'rate';
    }

    state.shownAt.push(now);
    showToast(ev, sev);
    notifyIfWanted(ev, sev);
    return 'shown';
  }

  function scheduleAggregate() {
    if (state.pendingAggregate <= AGGREGATE_AFTER) return; // ordinary drops: no summary needed
    if (state.aggregateTimer) return;
    // At most one summary per RATE_WINDOW_MS: a sustained storm would otherwise
    // arm a fresh 2s timer the instant the previous one fired, turning the
    // summary itself into a flood.
    var since = Date.now() - (state.lastAggregateAt || 0);
    var wait = Math.max(AGGREGATE_DEBOUNCE_MS, RATE_WINDOW_MS - since);
    state.aggregateTimer = setTimeout(function () {
      state.aggregateTimer = null;
      var n = state.pendingAggregate;
      state.pendingAggregate = 0;
      state.lastAggregateAt = Date.now();
      if (n <= AGGREGATE_AFTER) return;
      showAggregate(n);
    }, wait);
  }

  // ── Rendering ──────────────────────────────────────────────────────────────
  function stack() { return document.getElementById('chToastStack'); }

  function showToast(ev, sev) {
    var host = stack();
    if (!host) return;
    while (state.visible.length >= MAX_VISIBLE) {
      var old = state.visible.shift();
      state.suppressed.retirement++;
      removeToast(old);
    }
    var el = document.createElement('div');
    el.className = 'toast toast-' + sev.toLowerCase();
    el.setAttribute('data-severity', sev);
    if (ev.component) el.setAttribute('data-component', String(ev.component));
    // Correlation attributes: the event id and its own timestamp stay on the
    // card, so an operator (or an acceptance probe) can tie a toast back to the
    // frame that produced it — including proving the frame arrived AFTER the
    // page connected (a live push, not a reload).
    if (ev.id != null) el.setAttribute('data-event-id', String(ev.id));
    if (ev.created_at) el.setAttribute('data-created-at', String(ev.created_at));

    var head = document.createElement('div');
    head.className = 'toast-head';

    var tag = document.createElement('span');
    tag.className = 'toast-sev';
    tag.textContent = sev;
    head.appendChild(tag);

    var comp = document.createElement('span');
    comp.className = 'toast-comp';
    comp.textContent = String(ev.component || 'scheduler');
    head.appendChild(comp);

    var when = document.createElement('span');
    when.className = 'toast-when';
    when.textContent = clockLabel(ev.created_at);
    head.appendChild(when);

    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-x';
    close.setAttribute('aria-label', 'Dismiss notification');
    close.textContent = '\u00d7';
    head.appendChild(close);

    el.appendChild(head);

    var target = resolveTarget(ev);
    var msg = document.createElement('div');
    msg.className = 'toast-msg';
    if (target) {
      var a = document.createElement('a');
      a.className = 'toast-link';
      a.href = target.url;
      a.textContent = String(ev.message || '(no message)');
      msg.appendChild(a);
    } else {
      msg.textContent = String(ev.message || '(no message)');
    }
    el.appendChild(msg);

    var foot = document.createElement('div');
    foot.className = 'toast-foot';
    foot.textContent = target ? '\u2192 ' + target.url + ' (' + target.why + ')' : 'no lane/tick target on this event';
    el.appendChild(foot);

    if (target) {
      // The anchor above gives keyboard/middle-click semantics; this makes the
      // whole card a click target, which is what a toast affords. A click ON
      // the anchor is left to the anchor itself — navigating twice would abort
      // the first navigation for no benefit.
      el.classList.add('toast-clickable');
      el.addEventListener('click', function (e) {
        if (e.target && e.target.closest && e.target.closest('.toast-x')) return;
        if (e.target && e.target.closest && e.target.closest('a.toast-link')) return;
        if (typeof window.getSelection === 'function' && String(window.getSelection())) return;
        window.location.href = target.url;
      });
    }

    close.addEventListener('click', function (e) {
      e.stopPropagation();
      removeToast(el);
    });

    host.appendChild(el);
    state.visible.push(el);
    state.shown++;
    state.recentShown.push({
      id: ev.id != null ? ev.id : null,
      severity: sev,
      component: String(ev.component || ''),
      message: String(ev.message || '').slice(0, 160)
    });
    if (state.recentShown.length > REVIEW_SHOWN_REMEMBER) state.recentShown.shift();
    armDismiss(el, DISMISS_MS[sev] || DISMISS_MS.INFO);
    refreshPanel();
  }

  // NOTE: state.shown counts EVENT toasts only. The aggregate summary card is
  // deliberately NOT counted there (it is not an event's toast) — it is
  // recorded in recentShown with severity AGGREGATE so the page's toast record
  // stays complete.
  function showAggregate(n) {
    var host = stack();
    if (!host) return;
    var el = document.createElement('div');
    el.className = 'toast toast-aggregate';
    el.setAttribute('data-severity', 'AGGREGATE');

    var head = document.createElement('div');
    head.className = 'toast-head';
    var tag = document.createElement('span');
    tag.className = 'toast-sev';
    tag.textContent = 'SUMMARY';
    head.appendChild(tag);
    var comp = document.createElement('span');
    comp.className = 'toast-comp';
    comp.textContent = 'event stream';
    head.appendChild(comp);
    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'toast-x';
    close.setAttribute('aria-label', 'Dismiss summary');
    close.textContent = '\u00d7';
    head.appendChild(close);
    el.appendChild(head);

    var msg = document.createElement('div');
    msg.className = 'toast-msg';
    msg.textContent = 'throttled: +' + n + ' more events suppressed in the last ' +
      Math.round(RATE_WINDOW_MS / 1000) + 's — ' + RATE_MAX + ' toasts per window is the budget. ' +
      'Open /tape for the raw feed.';
    el.appendChild(msg);

    var foot = document.createElement('div');
    foot.className = 'toast-foot';
    foot.textContent = 'throttled — individual events were dropped, not lost: /api/v1/events/stream replays them';
    el.appendChild(foot);

    close.addEventListener('click', function () { removeToast(el); });
    while (state.visible.length >= MAX_VISIBLE) { removeToast(state.visible[0]); }
    host.appendChild(el);
    state.visible.push(el);
    // The summary is part of the page's toast record: it is what the operator
    // saw instead of the dropped events.
    state.recentShown.push({ id: null, severity: 'AGGREGATE', component: 'event stream', message: '+' + n + ' more suppressed' });
    if (state.recentShown.length > REVIEW_SHOWN_REMEMBER) state.recentShown.shift();
    armDismiss(el, DISMISS_MS.MEDIUM);
  }

  function removeToast(el) {
    if (!el) return;
    if (el._chTimer) { clearTimeout(el._chTimer); el._chTimer = null; }
    var i = state.visible.indexOf(el);
    if (i >= 0) state.visible.splice(i, 1);
    if (el.parentNode) el.parentNode.removeChild(el);
  }

  // Hover (or keyboard focus) pauses the countdown; leaving resumes with the
  // REMAINING time, so a toast that has been up for 10s does not restart its
  // full 12s because the pointer crossed it.
  function armDismiss(el, ms) {
    var remaining = ms, startedAt = 0;
    function start() {
      startedAt = Date.now();
      el._chTimer = setTimeout(function () { removeToast(el); }, remaining);
    }
    function pause() {
      if (!el._chTimer) return;
      clearTimeout(el._chTimer);
      el._chTimer = null;
      remaining = Math.max(1500, remaining - (Date.now() - startedAt));
    }
    function resume() { if (!el._chTimer && el.parentNode) start(); }
    el.addEventListener('mouseenter', pause);
    el.addEventListener('mouseleave', resume);
    el.addEventListener('focusin', pause);
    el.addEventListener('focusout', resume);
    start();
  }

  function clockLabel(utc) {
    var d = utc ? new Date(utc) : new Date();
    if (isNaN(d.getTime())) d = new Date();
    function p(n) { return (n < 10 ? '0' : '') + n; }
    return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  // ── Click-through target resolution ────────────────────────────────────────
  // Ordered, and each step says WHY it matched (the label is rendered in the
  // toast footer, so the operator can see the reason for the destination).
  function resolveTarget(ev) {
    if (!ev) return null;
    var d = parseDetails(ev.details);

    if (d) {
      var tick = firstString(d, ['tick_id', 'tickId', 'tick', 'run_id', 'runId']);
      if (tick && looksLikeTick(tick)) return { url: '/ticks/' + encodeURIComponent(tick), why: 'details.tick_id' };
      // The lane value must be exactly ONE lane-shaped token. Producers put
      // human descriptions in some of these fields — the api.auth refusal
      // events carry target:"project 9router-pm bump" — and a description is
      // not a lane: linking it produced /projects/project%209router-pm%20bump.
      // A value that fails the shape check falls through to the message rules
      // below (which then resolve the route the event echoed).
      var lane = firstString(d, ['project', 'project_name', 'lane', 'lane_name', 'projectName', 'target']);
      if (lane && laneShaped(lane)) {
        return { url: '/projects/' + encodeURIComponent(lane), why: 'details.project' };
      }
    }

    var m = String(ev.message == null ? '' : ev.message);

    // A dashboard route echoed in the message — auth refusals and API errors
    // name the path they refused ("auth: refused-no-credential POST
    // /api/v1/projects/warpfs-pm/bump"), and a route the operator already has
    // is an exact target, not a guess.
    var routeTick = m.match(/\/ticks\/([A-Za-z0-9][A-Za-z0-9._:-]*)/);
    if (routeTick && looksLikeTick(routeTick[1])) {
      return { url: '/ticks/' + routeTick[1], why: 'message route' };
    }
    var routeLane = m.match(/\/projects\/([A-Za-z0-9][A-Za-z0-9._-]*)/);
    if (routeLane) {
      return { url: '/projects/' + routeLane[1], why: 'message route' };
    }

    var laneMatch = m.match(/lane held by budget[^:]*:\s*([^\s\u2014,]+)/i) ||
      m.match(/(?:project|lane)\s+starved:\s*([^\s\u2014,]+)/i) ||
      m.match(/board write on\s+([^\s\u2014,]+)/i) ||
      m.match(/(?:project|lane)\s+([A-Za-z0-9][A-Za-z0-9._-]*)/i);
    if (laneMatch && laneMatch[1]) {
      var laneName = trimPunct(laneMatch[1]);
      if (knownLane(laneName) || laneShaped(laneName)) {
        return { url: '/projects/' + encodeURIComponent(laneName), why: 'message' };
      }
    }

    // Tick ids in this fleet are "<project>-YYYY-MM-DD-HH-MM-SS" (or a
    // schedulerId/tick pair), i.e. they contain BOTH a hyphen and a digit.
    // That guard is what keeps "last tick 47h59m9s ago" from becoming a link.
    var tickMatch = m.match(/\btick[ =:]+([A-Za-z0-9][A-Za-z0-9._:-]*)/i);
    if (tickMatch && looksLikeTick(tickMatch[1])) {
      return { url: '/ticks/' + encodeURIComponent(trimPunct(tickMatch[1])), why: 'message' };
    }
    return null;
  }

  function parseDetails(details) {
    if (!details) return null;
    if (typeof details === 'object') return details;
    try {
      var v = JSON.parse(details);
      return v && typeof v === 'object' ? v : null;
    } catch (err) { return null; }
  }

  function firstString(obj, keys) {
    for (var i = 0; i < keys.length; i++) {
      var v = obj[keys[i]];
      if (typeof v === 'string' && v.trim() !== '') return v.trim();
    }
    return '';
  }

  function looksLikeTick(v) {
    var s = String(v);
    return /[0-9]/.test(s) && s.indexOf('-') > 0 && s.length >= 6;
  }

  function trimPunct(v) {
    return String(v).replace(/[.,;:)\]]+$/, '');
  }

  function laneShaped(v) {
    // Lane names in this fleet are slugs: lowercase/digits with . _ -, and they
    // never look like a bare number or a duration.
    return /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(v) && !/^[0-9]+$/.test(v) && !/^\d+[hms]/.test(v);
  }

  function knownLane(name) {
    var links = document.querySelectorAll('a[href^="/projects/"]');
    for (var i = 0; i < links.length; i++) {
      var href = links[i].getAttribute('href') || '';
      if (decodeURIComponent(href.slice('/projects/'.length)) === name) return true;
    }
    return false;
  }

  // ── Browser notifications ──────────────────────────────────────────────────
  function notificationsSupported() { return typeof window.Notification === 'function'; }

  function notifState() {
    if (!notificationsSupported()) return 'unsupported';
    if (!window.isSecureContext) return 'insecure';
    return window.Notification.permission; // 'default' | 'granted' | 'denied'
  }

  // The ONLY call site of requestPermission: the settings button's click.
  function enableNotifications() {
    var st = notifState();
    if (st === 'insecure' || st === 'unsupported') { refreshNotifUI(); return; }
    if (st === 'granted') { settings.enabled = !settings.enabled; saveSettings(settings); refreshNotifUI(); return; }
    if (st === 'denied') { refreshNotifUI(); return; }
    window.Notification.requestPermission().then(function (perm) {
      settings.enabled = perm === 'granted';
      saveSettings(settings);
      refreshNotifUI();
    }).catch(function (err) {
      state.lastError = String(err);
      refreshNotifUI();
    });
  }

  function notifyIfWanted(ev, sev) {
    if (!settings.enabled) return;           // opt-in (2): the operator's own switch
    if (notifState() !== 'granted') return;  // opt-in (1): permission, asked only on click
    var tier = (sev === 'CRITICAL' || sev === 'HIGH') ? 'HIGH' : sev;
    if (!settings.notify[tier]) return;
    try {
      var n = new window.Notification(sev + ' \u00b7 ' + String(ev.component || 'scheduler'), {
        body: String(ev.message || '') + '\n' + clockLabel(ev.created_at),
        // tag collapses OS-level duplicates of the same news automatically.
        tag: dedupeKey(ev),
        silent: tier === 'INFO'
      });
      n.onclick = function () {
        try { window.focus(); } catch (err) { /* focus is best-effort */ }
        var t = resolveTarget(ev);
        if (t) window.location.href = t.url;
        try { n.close(); } catch (err) { /* already closed */ }
      };
    } catch (err) {
      state.lastError = String(err);
    }
  }

  function refreshNotifUI() {
    var button = document.getElementById('chNotifEnable');
    var note = document.getElementById('chNotifSec');
    if (!note) return;
    var st = notifState();
    var text;
    if (st === 'unsupported') {
      text = 'This browser has no Notification API, so browser notifications are unavailable here. The in-page toasts above work normally.';
    } else if (st === 'insecure') {
      text = 'Browser notifications require a SECURE CONTEXT (isSecureContext = false on this origin). ' +
        'Serve the dashboard over https:// — or reach it as http://localhost / http://127.0.0.1, which browsers treat as secure — ' +
        'and the button below becomes available. Until then the in-page toasts are the only channel.';
    } else if (st === 'denied') {
      text = 'Permission is BLOCKED (Notification.permission = "denied"). This page cannot re-prompt: ' +
        'clear the site permission in the browser\u2019s site settings to change it. In-page toasts keep working.';
    } else if (st === 'default') {
      text = 'Not requested yet. Clicking the button asks the browser once \u2014 nothing is requested on page load.';
    } else if (settings.enabled) {
      text = 'Enabled for: ' + enabledTiers() + '. Click the button to turn notifications off (permission stays granted).';
    } else {
      text = 'Permission granted, but notifications are OFF until you enable them here.';
    }
    note.textContent = text;
    if (button) {
      var usable = (st === 'default' || st === 'granted');
      button.disabled = !usable;
      button.textContent = st === 'granted' && settings.enabled ? 'Disable notifications' : 'Enable notifications';
      button.title = usable ? '' : text;
      button.hidden = (st === 'insecure' || st === 'unsupported');
    }
  }

  function enabledTiers() {
    var on = NOTIFY_TIERS.filter(function (t) { return settings.notify[t]; });
    return on.length ? on.join(', ') + (settings.notify.HIGH ? ' (incl. CRITICAL)' : '') : 'nothing';
  }

  // ── Settings panel ─────────────────────────────────────────────────────────
  function refreshPanel() {
    var feed = document.getElementById('chToastFeed');
    if (feed) {
      feed.textContent = state.connected
        ? 'SSE live \u00b7 ' + state.events + ' events seen'
        : 'SSE not connected \u2014 the page\u2019s auto-refresh still updates the tables';
      feed.className = 'tp-note ' + (state.connected ? 'tp-up' : 'tp-down');
    }
    var counts = document.getElementById('chToastCounts');
    if (counts) {
      var s = state.suppressed;
      counts.textContent = state.shown + ' toast' + (state.shown === 1 ? '' : 's') +
        ' \u00b7 ' + state.events + ' events \u00b7 dropped: ' + (s.routine + s.severity + s.dedupe + s.rate + s.replay) +
        ' (routine ' + s.routine + ', below-threshold ' + s.severity + ', duplicate ' + s.dedupe +
        ', rate-capped ' + s.rate + ', replay ' + s.replay + ')' +
        ' \u00b7 cap ' + RATE_MAX + '/' + Math.round(RATE_WINDOW_MS / 1000) + 's';
    }
  }

  function wirePanel() {
    var btn = document.getElementById('chToastCtlBtn');
    var panel = document.getElementById('chToastPanel');
    if (btn && panel) {
      btn.addEventListener('click', function () {
        var open = panel.hidden;
        panel.hidden = !open;
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
        refreshNotifUI();
        refreshPanel();
      });
    }

    var sev = document.getElementById('chToastSev');
    if (sev) {
      sev.value = settings.threshold;
      sev.addEventListener('change', function () {
        if (THRESHOLDS.indexOf(sev.value) >= 0) settings.threshold = sev.value;
        saveSettings(settings);
      });
    }

    NOTIFY_TIERS.forEach(function (tier) {
      var box = document.getElementById('chNotif' + tier);
      if (!box) return;
      box.checked = !!settings.notify[tier];
      box.addEventListener('change', function () {
        settings.notify[tier] = box.checked;
        saveSettings(settings);
        refreshNotifUI();
      });
    });

    var enable = document.getElementById('chNotifEnable');
    if (enable) enable.addEventListener('click', enableNotifications);

    var test = document.getElementById('chToastTest');
    if (test) {
      test.addEventListener('click', function () {
        // Local-only preview of the styling; it does NOT touch the feed and is
        // labelled as a test in the toast body so it can never be mistaken for
        // a real fleet event.
        var probe = { id: -1, severity: 'HIGH', component: 'toast-test', message: 'test toast — styling preview (not a fleet event)', details: '', created_at: new Date().toISOString() };
        showToast(probe, 'HIGH');
      });
    }

    refreshNotifUI();
    refreshPanel();
    setInterval(refreshPanel, 2000); // cheap counter refresh; no feed work here
  }

  // ── SSE ────────────────────────────────────────────────────────────────────
  function connect() {
    if (typeof window.EventSource !== 'function') {
      setFeedNote('EventSource unsupported in this browser \u2014 the page\u2019s auto-refresh still updates the tables');
      return;
    }
    var es;
    try { es = new window.EventSource(FEED_URL); } catch (err) {
      state.lastError = String(err);
      setFeedNote('SSE unavailable: ' + state.lastError);
      return;
    }
    es.onopen = function () { state.connected = true; refreshPanel(); };
    es.onerror = function () {
      // EventSource reconnects on its own; this is a state note, not a failure
      // the operator must act on.
      state.connected = false;
      refreshPanel();
    };
    es.onmessage = function (m) {
      var ev;
      try { ev = JSON.parse(m.data); } catch (err) { state.badFrames++; return; }
      handleEvent(ev);
    };
  }

  function setFeedNote(text) {
    var feed = document.getElementById('chToastFeed');
    if (feed) { feed.textContent = text; feed.className = 'tp-note tp-down'; }
  }

  // ── Boot ───────────────────────────────────────────────────────────────────
  function boot() {
    wirePanel();
    connect();
  }

  // Diagnostics surface (see OBSERVABILITY above): stats for acceptance
  // evidence, emit/inject for driving the policy with captured frames.
  window.CHToast = {
    version: 'SCHED-GAP-1599',
    policy: {
      FEED_URL: FEED_URL, DEDUPE_WINDOW_MS: DEDUPE_WINDOW_MS, RATE_WINDOW_MS: RATE_WINDOW_MS,
      RATE_MAX: RATE_MAX, MAX_VISIBLE: MAX_VISIBLE, AGGREGATE_AFTER: AGGREGATE_AFTER,
      DISMISS_MS: DISMISS_MS, DEFAULT_THRESHOLD: DEFAULT_THRESHOLD, QUIET: QUIET.map(String)
    },
    stats: function () {
      return {
        connected: state.connected, events: state.events, shown: state.shown, badFrames: state.badFrames,
        visible: state.visible.length, suppressed: JSON.parse(JSON.stringify(state.suppressed)),
        recentShown: state.recentShown.slice(),
        lastDecisions: state.lastDecisions.slice(),
        threshold: settings.threshold, notifyEnabled: settings.enabled, notifyTiers: cloneNotify(settings.notify),
        notifState: notifState(), secureContext: !!window.isSecureContext, lastId: state.lastId, lastError: state.lastError
      };
    },
    emit: handleEvent,
    resolve: resolveTarget,
    // Test-only: forget dedupe/rate history (the visible toasts are untouched).
    reset: function () {
      state.dedupeKeys = new Map();
      state.shownAt = [];
      state.pendingAggregate = 0;
      state.lastAggregateAt = 0;
      state.lastId = null;
      state.suppressed = { routine: 0, severity: 0, dedupe: 0, rate: 0, replay: 0, retirement: 0 };
      state.events = 0;
      state.shown = 0;
      state.recentShown = [];
      state.lastDecisions = [];
    },
    setThreshold: function (t) {
      if (THRESHOLDS.indexOf(t) >= 0) { settings.threshold = t; saveSettings(settings); }
      return settings.threshold;
    }
  };

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
