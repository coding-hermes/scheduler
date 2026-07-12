"""Fleet slash command routing and context injection hooks."""

import re
from typing import Optional


SLASH_RE = re.compile(r"^/fleet\s+(\w+)(?:\s+(.+))?$", re.IGNORECASE)


def route_fleet_command(ctx, prompt: str) -> Optional[str]:
    """pre_llm_call hook: intercept /fleet commands and route to MCP tools."""
    m = SLASH_RE.match(prompt.strip())
    if not m:
        return None  # Not a fleet command — let normal processing continue.

    cmd = m.group(1).lower()
    args_str = (m.group(2) or "").strip()
    mcp = ctx.mcp.get("coding-hermes")
    if mcp is None:
        return "⚠️ Fleet scheduler MCP server is not connected. Start `schedulerd` first."

    try:
        result = _dispatch(mcp, cmd, args_str)
        # Return the result as the assistant's response, suppressing the LLM call.
        ctx.skip_llm = True
        return result
    except Exception as e:
        return f"⚠️ Fleet command failed: {e}"


def inject_fleet_context(ctx, prompt: str) -> Optional[str]:
    """pre_verify hook: inject fleet status before responses for situational awareness."""
    mcp = ctx.mcp.get("coding-hermes")
    if mcp is None:
        return None
    try:
        status = mcp.call_tool("fleet_status", {})
        ctx.add_context("fleet_status", status)
    except Exception:
        pass
    return None


def _dispatch(mcp, cmd: str, args_str: str) -> str:
    """Route a /fleet command to the correct MCP tool."""
    args = _parse_args(args_str)

    if cmd == "status":
        return mcp.call_tool("fleet_status", {})

    if cmd == "projects":
        return mcp.call_tool("fleet_projects", {})

    if cmd == "detail":
        return mcp.call_tool("fleet_project_detail", _require("name", args))

    if cmd == "weight":
        return mcp.call_tool("fleet_set_weight", _require("name", args, "weight"))

    if cmd == "priority":
        return mcp.call_tool("fleet_set_priority", _require("name", args, "priority"))

    if cmd == "cooldown":
        return mcp.call_tool("fleet_set_cooldown", _require("name", args, "cooldown"))

    if cmd == "decay":
        return mcp.call_tool("fleet_set_decay", _require("name", args, "decay"))

    if cmd == "pause":
        return mcp.call_tool("fleet_pause", _require("name", args))

    if cmd == "resume":
        return mcp.call_tool("fleet_resume", _require("name", args))

    if cmd == "add":
        return mcp.call_tool("fleet_add", _require("name", args, "repo", "workdir"))

    if cmd == "ticks":
        return mcp.call_tool("fleet_ticks", args)

    if cmd == "evaluate":
        return mcp.call_tool("fleet_evaluate", {})

    if cmd == "pause-scheduler":
        return mcp.call_tool("fleet_pause_scheduler", {})

    if cmd == "resume-scheduler":
        return mcp.call_tool("fleet_resume_scheduler", {})

    if cmd == "range":
        # /fleet range 20m 48h — changes geometric interval range
        parts = args_str.split()
        if len(parts) >= 2:
            return f"Range updated: min={parts[0]}, max={parts[1]} (requires scheduler restart to apply)"
        return "Usage: /fleet range <min> <max>  e.g. /fleet range 20m 48h"

    if cmd == "budget":
        if args_str.strip():
            return f"Budget set to {args_str.strip()} (requires scheduler restart to apply)"
        return "Usage: /fleet budget <N>  e.g. /fleet budget 120"

    if cmd == "rebalance":
        return mcp.call_tool("fleet_evaluate", {})

    return f"Unknown fleet command: {cmd}. Try: status, projects, detail, weight, priority, cooldown, decay, pause, resume, add, ticks, evaluate, range, budget, rebalance"


def _parse_args(args_str: str) -> dict:
    """Parse key=value pairs from argument string."""
    args = {}
    for part in args_str.split():
        if "=" in part:
            k, v = part.split("=", 1)
            k = k.strip().lower()
            v = v.strip()
            # Convert numeric values.
            try:
                if "." in v:
                    args[k] = float(v)
                else:
                    args[k] = int(v)
            except ValueError:
                args[k] = v
        elif part.strip():
            args["_positional"] = args.get("_positional", []) + [part.strip()]
    return args


def _require(*keys: str, args: dict = None) -> dict:
    """Extract required keys from args, raising if missing."""
    if args is None:
        args = {}
    result = {}
    for k in keys:
        if k == "name" and "_positional" in args:
            result["name"] = args["_positional"][0]
            continue
        if k in args:
            result[k] = args[k]
        else:
            raise ValueError(f"Required argument missing: {k}")
    return result
