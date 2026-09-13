#!/usr/bin/env python3
"""dagger-role-report.py — human report for dagger ROLE ticks (qa, dogfood,
duckbrain-sync, pm, bankai, plus-ultra, sub-scout).

Replaces the raw node-table dump the role driver used to deliver
(`resolve_targets tool ✅ {"mode":"targeted",...}` walls of truncated JSON).

Sources everything from the role run DB checkpoints:
  - headline verdict derived from the loop results (worst severity / counts)
  - per-target readable lines: verdict, summary, findings with severity
  - sync: verified counts (the only role where a mismatch is the story)
  - pm: proposal titles + escalation flag
Footer points at the full run log for depth.

Args: <role_run_db> <run_log> <pipeline> <project> [tick_id]
"""
import json
import os
import re
import sqlite3
import sys
import textwrap

WRAP = 96


def node_outputs(run_db):
    out = {}
    if not os.path.exists(run_db):
        return out
    try:
        db = sqlite3.connect(f"file:{run_db}?mode=ro", uri=True)
        for node_id, raw in db.execute(
            "SELECT node_id, output FROM checkpoints ORDER BY step"
        ).fetchall():
            node = node_id.split("/")[-1]
            if not raw:
                continue
            try:
                obj = json.loads(raw)
                if isinstance(obj, str):
                    obj = json.loads(obj)
                out[node] = obj
            except Exception:
                pass
        db.close()
    except Exception:
        pass
    return out


def iter_results(run_db, final_nodes):
    """Per-target results live in forEach template-iteration checkpoints.

    Multiple iterations share the same short node name; node_outputs keeps
    only the last, so this re-reads raw rows and collects every dict whose
    node matches one of the per-target final nodes (qa=file_findings,
    dogfood=write_findings, sync=verify_key), ordered by step.
    """
    found = []
    if not run_db or not os.path.exists(run_db):
        return found
    try:
        db = sqlite3.connect(f"file:{run_db}?mode=ro", uri=True)
        for node_id, raw in db.execute(
            "SELECT node_id, output FROM checkpoints ORDER BY step"
        ).fetchall():
            node = node_id.split("/")[-1]
            if node not in final_nodes or not raw:
                continue
            try:
                obj = json.loads(raw)
                if isinstance(obj, str):
                    obj = json.loads(obj)
            except Exception:
                continue
            if isinstance(obj, dict) and ("t" in obj or "key" in obj):
                found.append(obj)
        db.close()
    except Exception:
        pass
    return found


def fmt_duration(raw):
    m = re.match(r"(?:(\d+)h)?(?:(\d+)m)?([\d.]+)s", raw or "")
    if not m:
        return raw or "?"
    h, mn, s = m.group(1), m.group(2), float(m.group(3))
    if h:
        return f"{h}h{mn}m{round(s)}s" if mn else f"{h}h{round(s)}s"
    if mn:
        return f"{mn}m{round(s)}s"
    return f"{round(s)}s"


def wrap(text, indent=""):
    return textwrap.fill(str(text), width=WRAP, initial_indent=indent,
                         subsequent_indent=indent + "  ")


def emit(t):
    print(t)


def main():
    run_db, run_log, pipeline, project = sys.argv[1:5]
    log = open(run_log, errors="replace").read() if os.path.exists(run_log) else ""
    total = re.search(r"Total time: ([0-9hms.]+) \| Passed: (\d+) \| Failed: (\d+)", log)
    dur = fmt_duration(total.group(1) if total else "?")
    failed = total.group(3) if total else "?"

    nodes = node_outputs(run_db)
    rep = nodes.get("report", {})
    report_text = rep.get("report") if isinstance(rep, dict) else None

    parts = [pipeline.upper()]
    if project and project not in (pipeline, "qa-audit", "dogfooding"):
        parts.append(project)
    parts.append(dur)
    if failed not in ("?", "0"):
        parts.append(f"⚠ {failed} failed node(s)")

    ITER_NODE = {"qa": {"file_findings"}, "dogfooding": {"write_findings"},
                 "duckbrain-sync": {"verify_key"}}
    if pipeline in ITER_NODE:
        results = iter_results(run_db, ITER_NODE[pipeline])
    else:
        results = []

    findings_tot = 0
    sev_counts = {}

    if pipeline in ("qa", "dogfooding"):
        done = [r for r in results if r and not r.get("target_skipped") and not r.get("skipped")]
        skipped = [r for r in results if r and (r.get("target_skipped") or r.get("skipped"))]
        body = []
        for r in done:
            name = r.get("project") or (r.get("t") or {}).get("project") or "?"
            verdict = r.get("verdict") or "?"
            finds = r.get("findings") or []
            findings_tot += len(finds)
            for f in finds:
                sv = str(f.get("severity") or "?")
                sev_counts[sv] = sev_counts.get(sv, 0) + 1
            summary = re.sub(r"\s+", " ", str(r.get("summary") or "")).strip()
            line = f"{name} — {verdict}"
            if summary:
                line += f": {summary}"
            body.append(wrap(line, indent="• "))
            for f in finds[:6]:
                body.append(wrap(f"[{f.get('severity') or '?'}] {f.get('title') or ''}".strip(),
                                 indent="    "))
            if pipeline == "dogfooding":
                promise = (r.get("learn") or {}).get("promise")
                if promise:
                    promise = re.sub(r"\s+", " ", str(promise)).strip()
                    if len(promise) > 220:
                        promise = promise[:217] + "…"
                    body.append(wrap(f"promise: {promise}", indent="    "))
        for r in skipped:
            name = r.get("project") or (r.get("t") or {}).get("project") or "?"
            reason = re.sub(r"\s+", " ", str(r.get("summary") or r.get("reason") or "skipped")).strip()
            body.append(wrap(f"{name} — skipped: {reason}", indent="◦ "))
        icon = "🟢"
        if sev_counts.get("P0"):
            icon = "🔴"
        elif sev_counts.get("P1"):
            icon = "🟠"
        elif findings_tot:
            icon = "🟡"
        sev = " ".join(f"{k}:{v}" for k, v in sorted(sev_counts.items())) if sev_counts else "0"
        parts.insert(0, icon)
        emit(" · ".join([p for p in parts if p]))
        emit("")
        emit(f"{len(done)} audited · {findings_tot} findings ({sev})" if pipeline == "qa"
             else f"{len(done)} exercised · {findings_tot} findings ({sev})"
             + (f" · {len(skipped)} skipped" if skipped else ""))
        if not done and not skipped:
            emit(wrap("No targets ran this tick — see the run log for why."))
        for line in body:
            emit(line)
    elif pipeline == "duckbrain-sync":
        bad = [r for r in results if not r or (r.get("wrote") and not r.get("verified"))]
        verified = [r for r in results if r and r.get("verified")]
        icon = "🔴" if bad else "🟢"
        parts.insert(0, icon)
        emit(" · ".join([p for p in parts if p]))
        emit("")
        emit(f"{len(verified)}/{len(results)} keys verified" +
             (" (dry-run — no writes)" if any(r and r.get("dry") for r in results) else ""))
        for r in bad:
            emit(wrap(f"MISMATCH: {r.get('key') if r else '(iteration lost)'}",
                      indent="• "))
    elif pipeline == "pm":
        per = nodes.get("persist", {})
        proposals = per.get("proposals") if isinstance(per, dict) else None
        n = len(proposals) if isinstance(proposals, list) else None
        escalate = "ESCALATION NEEDED" in (report_text or "")
        icon = "🔴" if escalate else ("🟡" if n else "🟢")
        parts.insert(0, icon)
        emit(" · ".join([p for p in parts if p]))
        emit("")
        if n:
            emit(f"{n} proposal(s) filed on the board:")
            for p in proposals[:6]:
                emit(wrap(f"[{p.get('priority') or '?'}] {p.get('title') or ''}".strip(),
                          indent="• "))
        elif isinstance(proposals, list):
            emit("No new proposals this tick (nothing new worth filing).")
        else:
            emit(re.sub(r"\s+", " ", report_text or "PM recon completed.").strip())
        if escalate:
            emit("")
            emit(wrap("⚠ Escalation: stalled items repeated across runs — needs operator attention."))
    else:
        # bankai / plus-ultra / sub-scout / fallback: cleaned report text
        icon = "⚠️" if failed not in ("?", "0") else "🟢"
        parts.insert(0, icon)
        emit(" · ".join([p for p in parts if p]))
        if report_text:
            emit("")
            emit(report_text)
        else:
            emit("")
            emit("(no report payload — see the run log)")

    emit("")
    emit(f"more: {run_log}" if run_log else "more: (run log unavailable)")


if __name__ == "__main__":
    main()
