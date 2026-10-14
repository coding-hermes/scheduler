# Repository Traceability Doctrine

Every board row, close-out event, and worker report in this repository carries a
`ch:trace` marker: a single-line, machine-parseable pointer set that binds a claim
to the row it closes, the spec it implements, the evidence that proves it, and the
commit that landed it. The contract is deliberately small — a worker starting from
a fresh checkout must be able to resolve every pointer in a marker using only the
repository (and the artifacts the pointers name).

This file is the contract those markers cite
(`spec=docs/traceability-doctrine.md#marker`). When a marker and this doctrine
disagree, the marker is the bug: fix it, never re-interpret the contract to fit
the marker.

<a id="marker"></a>
## Marker grammar

A marker is one line of space-separated `key=value` tokens. Values are single
tokens: no unencoded spaces (use `-` or `:` inside a value), no quotes. The
one-line form is deliberate — markers survive JSONL boards, event logs, and
commit bodies without reformatting.

| Field | Meaning | Absent form |
|---|---|---|
| `row=` | Board row id the marker describes. Required on every marker. | — (never absent) |
| `spec=` | Spec or doctrine file (with `#anchor`) the work implements. | `none:<reason>` |
| `wave=` | Dispatch wave id + `#cell`, when the work came from a wave. | `none:serial-worker` |
| `test=` | Test symbol proving the behavior (`pkg/path/file.go::TestName`). | `none:<reason>` |
| `doc=` | Primary documentation artifact touched. | `none:<reason>` |
| `prompt=` | Worker prompt / skill file dispatched. | `none:<reason>` |
| `evidence=` | Primary evidence pointer: verdict id, artifact path, or run id. | `none:<reason>` |
| `witness=` | Independent verification claim (see Witness rules). | `none:<reason>` |
| `verdict=` | Judge verdict id (short sha) grading the work. | `none:<reason>` |
| `commit=` | Commit sha that landed the work. | `none:<reason>` |
| `memory=` | Memory / doc write-back recording the outcome. | `none:not-written` |

Every field must be present, either with a value or as `none:<reason>`. A field
silently omitted is a malformed marker.

## Value conventions

- `none:<reason>` — the field has no value, and the reason is stated in the value
  itself: `none:serial-worker`, `none:docs-only-no-test`, `none:not-written`.
  A bare `none` without a reason is invalid; an unexplained absence hides the gap
  it is supposed to disclose.
- `path#anchor` — file references carry an anchor when the claim names a section
  (`spec=docs/scheduler-rules.md#R3.3`).
- Evidence and verdict pointers name artifacts that exist or ids that resolve:
  a verdict id resolves in `.gitreins/history/`, a path resolves from the repo
  root, a commit sha resolves with `git show`.

## Evidence tiers

Work is graded on a four-tier ladder. The tier states how far the claim has been
pushed toward observable reality:

| Tier | Name | Meaning |
|---|---|---|
| T0 | exists | The artifact exists in the tree (a read/grep finds it). |
| T1 | imports | The artifact is wired in: other code references or imports it. |
| T2 | unit-tested | A named test exercises the artifact's contract and passes. |
| T3 | invoked-and-observed | The live surface was driven and the output observed and recorded. |

Rules:

- **T3 is the floor for done.** A row closes at T3 or with an explicit,
  reasoned exception recorded in the close-out.
- **T0–T2 alone never close a row.** Existing code, wired code, and green unit
  tests all describe the artifact, not its behavior on the live surface. Rows
  that stop at T2 are partial, not complete — the close-out says so.
- A T3 claim records what was invoked and what was observed: endpoint + response,
  dashboard render, log line, or CLI output — enough for a reader to re-run it.

## Witness rules

Every claim carries a witness: an independent, checkable pointer that the claim
happened as stated. No witness, no claim — the marker carries
`witness=none:<reason>` instead, and the reason is specific
(`none:tier2-judge-402-provider-balance`, not `none:skipped`).

Witness classes, in descending strength:

| Form | Meaning |
|---|---|
| `live:<locator>` | Observed live at a named surface (`live:gh-run-<id>`, `live:origin/main@<sha>`). |
| `commit:<sha>[@<ref>]` | The commit itself, on a named ref. |
| `github:<org>/<repo>@<sha>` | The commit on the public remote. |
| `path@origin:<path>` | The file as it exists at the remote. |
| `verdict:<id>` | A judge verdict bound to the work. |

A witness should not be the claim restated: `evidence=this event` or a pointer to
the marker's own row is a self-witness and only valid when nothing independent
exists — say why.

## Close-out block

Row closures and close-out events carry a three-line block on top of the marker:

```
STATUS: complete | partial | blocked
TIER: T0 | T1 | T2 | T3 (+ one clause: what was invoked and observed for T3)
EVIDENCE: <verdict id | artifact path | run id | commit sha>
```

- `STATUS: complete` requires `TIER: T3` (or the explicit exception), a resolvable
  `EVIDENCE:` pointer, and a witness per the rules above.
- `STATUS: partial` and `STATUS: blocked` name what remains and why, so the next
  tick inherits a handoff instead of a mystery.

## Worked example

```
ch:trace row=DOC-13 spec=docs/traceability-doctrine.md#marker wave=none:serial-worker test=none:docs-link-resolution doc=docs/traceability-doctrine.md evidence=<verdict-id> witness=commit:<sha>@wt/DOC-13 verdict=<verdict-id> commit=<sha> memory=none:not-written

STATUS: complete
TIER: T3 (doc rendered from repo checkout; root link resolves)
EVIDENCE: <verdict-id or commit sha>
```

## Cross-references

- [scheduler-rules.md](scheduler-rules.md) — the enforced/filed/tripwire status
  vocabulary this doctrine's evidence tiers complement: a rule is *enforced* at
  T3, *filed* means the row is still open.
- [design-decisions.md](design-decisions.md) — one line per decision; its claims
  follow the same witness discipline.
- [fleet-config-model.md](fleet-config-model.md) — every claim paired with the
  command that proves it; the marker is the per-row version of that pairing.
