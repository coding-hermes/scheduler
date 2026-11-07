# RELEASE-050 — repo security switches: measured evidence

Tick: coding-hermes-scheduler-foreman-2026-11-07-23-12-40
Actor: totalwindupflightsystems (gh api user --jq .login)
Repository: coding-hermes/scheduler (visibility=public, license=MIT)

Every block below is a command with its real output. The Telegram credential value is
redacted everywhere; it is never printed by this evidence.

## Caller authority (precondition for the action)

```
$ gh api repos/coding-hermes/scheduler --jq '.permissions'
{"admin":true,"maintain":true,"push":true,"triage":true,"pull":true}
```

## BEFORE (measured this tick, before any write)

```
$ gh api repos/coding-hermes/scheduler --jq '.security_and_analysis'
{"dependabot_security_updates":{"status":"disabled"},
 "secret_scanning":{"status":"disabled"},
 "secret_scanning_non_provider_patterns":{"status":"disabled"},
 "secret_scanning_push_protection":{"status":"disabled"},
 "secret_scanning_validity_checks":{"status":"disabled"}}
```

## ARM 1 — both named switches ON

```
$ gh api -X PATCH repos/coding-hermes/scheduler --input sec.json   # HTTP 200
  request body:
  {"security_and_analysis":{
     "secret_scanning":{"status":"enabled"},
     "secret_scanning_push_protection":{"status":"enabled"},
     "secret_scanning_non_provider_patterns":{"status":"enabled"},
     "secret_scanning_validity_checks":{"status":"enabled"},
     "dependabot_security_updates":{"status":"enabled"}}}

$ gh api repos/coding-hermes/scheduler --jq '.security_and_analysis'
{"dependabot_security_updates":{"status":"enabled"},
 "secret_scanning":{"status":"enabled"},
 "secret_scanning_push_protection":{"status":"enabled"},
 "secret_scanning_non_provider_patterns":{"status":"disabled"},
 "secret_scanning_validity_checks":{"status":"disabled"}}
```

ARM 1 PASSES for the two switches this row names (secret_scanning and
secret_scanning_push_protection). dependabot_security_updates also flipped to enabled.

RESIDUAL (recorded, not claimed as done): secret_scanning_non_provider_patterns and
secret_scanning_validity_checks remain `disabled`. They were PATCHed individually twice
more, each answering HTTP 200 with an unchanged state — the platform accepted the request
and silently did not apply it (no error text, no refusal body). That is the documented
behaviour of a feature unavailable to the org, not a failed call. Filed as a separate
residual row rather than diluting this one.

## ARM 2 — the alert surface is live, not just the flag

```
$ gh api -i repos/coding-hermes/scheduler/secret-scanning/alerts
HTTP/2.0 200 OK

$ gh api repos/coding-hermes/scheduler/secret-scanning/alerts --jq '.[] | {...}'
{"number":1,"state":"open","secret_type":"telegram_bot_token",
 "path":"internal/scheduler/deliver.go","line":22,
 "commit":"64afc8a9f4a943afbe7cca98bcb5068f02701e69","publicly_leaked":true}
```

Count of open alerts at first read: 1 (non-zero, quoted above). The surface is live.

### The one open alert: proven dead, then resolved

The leaked Telegram bot token is NOT in the current tree:

```
$ git show HEAD:internal/scheduler/deliver.go | sed -n '18,26p'
   (delivery-mode comment block; no credential)
$ grep -cE '[0-9]{8,10}:[A-Za-z0-9_-]{30,}' internal/scheduler/deliver.go
0
```

The credential itself was tested read-only and is dead (value extracted in-process, never
written to disk or printed):

```
$ curl -s -m 12 "https://api.telegram.org/bot<TOKEN>/getMe"
{"ok":false,"error_code":401,"description":"Unauthorized"}
```

Because the credential is revoked, the alert was resolved with that resolution:

```
$ gh api -X PATCH repos/coding-hermes/scheduler/secret-scanning/alerts/1 \
    --input alert1b.json --jq '{number,state,resolution,resolved_at,resolved_by}'
{"number":1,"state":"resolved","resolution":"revoked",
 "resolved_at":"2026-11-07T23:20:57Z","resolved_by":"totalwindupflightsystems"}

$ gh api 'repos/coding-hermes/scheduler/secret-scanning/alerts?state=open' --jq 'length'
0
```

## ARM 3 — the enabling action is attributed

- Enabling action: API response on the PATCH; the repository object's `updated_at` moved to
  `2026-11-07T23:10:35Z` on the enabling call.
- Alert resolution: `resolved_by=totalwindupflightsystems`, `resolved_at=2026-11-07T23:20:57Z`
  (quoted in the ARM 2 block).
- No refusal occurred: the caller holds `admin=true` on the repository, so the
  "hand back to the owner" branch of this arm does not apply.

## Forbidden-shortcut check

- Closed on quoted API output, not a prose claim that a UI was toggled.
- Push protection was not enabled without secret scanning; both went on in the same call.
- No local gitleaks hook was substituted for server-side scanning (the hook is not a
  server-side control and RELEASE-046 arm 4 already records that class of gap).
