# Skill Size Policy + Fresh-Context Management

> Board row: SCHED-GAP-067 (P2). Companion to `docs/fleet-cost-governance.md` §5.
> First application of this policy: `docs/skill-bloat-report-2026-08-24.md`
> (19 fleet skills split from ~100K to ≤50K, zero content loss).

## 1. Why this exists

Fleet telemetry (Jul 12–Aug 23): 16,259 ticks, 9.48B input tokens vs 73.5M
output tokens — a **129:1 input:output ratio**. The fleet does not pay for
writing; it pays for *reading*. Every tick re-reads its skill set on every LLM
call. A 100KB SKILL.md costs ~25K tokens **per call, per tick, per project** —
the single largest avoidable context cost in the fleet. Proven: the first
100K→48.6K split saved ~240M tokens/day (fleet-cost-governance §5).

## 2. Size tiers (enforced on every fleet skill)

| Size | Class | Action |
|---|---|---|
| ≤ 50 KiB (51,200 B) | OK | nothing |
| > 50 KiB | TRIM | move detail corpora to `references/` at next opportunity |
| > 100 KiB (102,400 B) | EMERGENCY | split immediately; a skill this size also trips the ~100K patch ceiling, making it uneditable by normal patch flows |

Thresholds are in **bytes**, measured on `SKILL.md` itself (`wc -c`), not
counting `references/`, `scripts/`, or `templates/` — those load lazily.

## 3. Two-tier architecture (Bane doctrine, 2026-08-23)

- **CORE skills** — general, public-ready knowledge. Must be ≤50K. No
  project-specific per-tick detail.
- **DATA skills** — project foreman-ops (`coding-hermes/*-foreman-ops` and
  fleet-infra skills). Hold project-specific operational knowledge. Still
  ≤50K in the SKILL.md body; the bulk lives in `references/`.

**Corpora that move to `references/<topic>.md`:** per-tick learnings
("Tick NNN learnings", "Light-audit probe corrections"), tick cadence logs,
standing-blocks lists, full pitfalls lists, task-queue snapshots, changelogs,
troubleshooting casebooks, E2E battery logs, support-file listings.

**What stays in SKILL.md:** frontmatter, identity/mission, board tooling,
live-state probes, gate batteries, deployment/restart recipes, pin/cooldown
rules, shipped-feature contracts, and a pointer section naming every
`references/` file it moved out.

### The rule: MOVE, never DELETE

A trimmed skill that lost detail fails the "don't lose anything" rule.
Every byte removed from SKILL.md must land in a `references/` file
(new file, or appended to the declared archive file when the skill already
has one — e.g. 9router's tick ledger, helios' tick-history). After a split:

1. Every `references/X.md` named in SKILL.md exists on disk (no dangling
   pointers introduced by the split).
2. Frontmatter (`--- name/description ---`) is byte-identical to before.
3. The no-loss invariant is checked mechanically: removing the inserted
   pointer block from the new SKILL.md must reproduce the exact kept
   content, and every moved range must appear verbatim in its refs file.
4. Never write back a literal `[truncated]` marker seen in a read tool's
   output — it is a display artifact, not file content.
5. Other agents edit skills concurrently: guard every split with
   expected-content checks at the cut lines, and never reformat the whole
   file — only remove the moved ranges and insert the pointer block.

## 4. Fresh-context / cache-miss control (tick-author doctrine)

Prompt caching prices a cache **miss** at 2–31× the hit price. The tick's
context prefix (system prompt + loaded skills + history) is the cached
object; protect it.

1. **Byte-stable context within a tick.** Don't swap skills, system-prompt
   files, or large injected blobs mid-tick. Every mutation invalidates the
   cache prefix and the next call re-pays the whole context at miss price.
2. **Batch LLM calls together.** Cache TTL is ~5 minutes. Reads, terminal
   probes, and file checks first; then LLM calls back-to-back. A call after
   a >5 min gap re-pays the entire prefix as a miss.
3. **Keep tick prompts as short as correctness allows.** The prompt and the
   loaded-skill list are re-paid per call; a 15-skill load list on a 100K
   skill set is the worst-case combination this policy exists to prevent.
4. **Prefer pointers over paste.** Reference `references/<file>.md` and let
   the agent lazy-load it only when the tick actually needs the detail
   (the same lazy-load rule 9router proved for template skills: the
   zero-call index scan beats persisting a 450KB blob that is never read).

## 5. Operational procedure (how to run an audit + split)

```bash
# 1. Audit (snapshot + top-40). Tags: startXXX / mid / after / final.
python3 /tmp/skill_audit_473.py <tag>     # writes /tmp/skill_audit_<tag>.json

# 2. Section map per skill (##-level sizes; finds the corpora)
python3 /tmp/skill_sections.py <category>/<skill-dir> [...]

# 3. Byte-exact split with guards + no-loss verification
python3 /tmp/skill_split_474.py <plan.json>
```

The splitter takes a plan JSON (line ranges + expected-section prefixes +
refs targets + pointer text), refuses to touch frontmatter, verifies the
no-loss invariant per skill, and aborts the run on the first drift/corruption
without writing that skill's SKILL.md. Plan files from the 2026-08-24 sweep:
`/tmp/skill_plan_474{,b,c}.json` (19 skills, 0 failures after guard fixes).

Verification after any split:

```bash
python3 /tmp/skill_audit_473.py after     # counts must drop
# per edited skill: size ≤51200, head -6 shows valid --- frontmatter,
# every references/*.md it names exists, hermes skills list still shows it
```

## 6. Scope boundaries

- Fleet = `coding-hermes/*` skills plus fleet-infra skills by name
  (`coding-hermes-cron`, `coding-hermes-jsonl-board-append`, etc.).
- Non-fleet skills (`research/*`, `note-taking/*`, `devops/*`, `creative/*`,
  …) are out of scope for fleet sweeps; list them in the report as follow-up
  candidates only, never edit them in a fleet task.
- Only ever edit `/home/kara/.hermes/skills` (the active profile) — never
  another profile's skills directory.
