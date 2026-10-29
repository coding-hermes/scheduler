# Hop liveness watchdog — a hop that dies politely must not look healthy

`ops/hop-liveness-watch.sh` watches the gating hops of the code factory from a
systemd timer. It exists because a hop can exit **cleanly** and stay dead with
nothing noticing.

## Why this exists (SCHED-GAP-16946)

`crier-fleet.service` exited with `status=0/SUCCESS` at 2026-10-28T23:03 and sat
inactive for **11 hours**. Three things conspired:

* `Restart=on-failure` never applies to a clean exit — the unit was *supposed*
  to stay down as far as systemd was concerned;
* no alert fired, because nothing was watching the hop;
* the first thing to notice was a human running `ops/cluster-chain-probe.sh` by
  hand.

A hop that dies politely is indistinguishable from a hop that was never needed.
That is the same "green but did nothing" failure class as a tick that exits 0
having changed nothing — one layer up.

## What it watches

| hop | scope | unit |
| --- | --- | --- |
| `router` | user | `task-router-proxy.service` |
| `crier` | user | `crier-fleet.service` |
| `scheduler` | user | `coding-hermes-scheduler.service` |
| `bunker` | system | `bunkerd.service` |

The table is `HOP_WATCH_HOPS` in the script (`hop:scope:unit`, space separated),
overridable from the environment so a new hop can be watched before the file is
edited.

The watcher reads the unit's **own** view of itself — `LoadState`,
`ActiveState`, `SubState`, `Result`, `ExecMainCode`, `ExecMainStatus`,
`StateChangeTimestampMonotonic` — and classifies:

| class | condition | alert? |
| --- | --- | --- |
| `up` | `ActiveState=active` | no (silent) |
| `down` | `ActiveState=inactive` or `failed` | yes, on transition |
| `transition` | `activating` / `deactivating` / `reloading` / `refreshing` | no (recorded) |
| `unknown` | `LoadState=not-found`, or the properties could not be read | **yes** — a missing unit is a finding, never silence |

Reason codes:

* `HOP_DOWN_CLEAN_EXIT` — the hop exited 0 (or systemd's `Result=success`) and
  stayed down. This is the 11h crier-fleet shape.
* `HOP_DOWN_FAILURE` — the hop exited non-zero, was killed, or dumped core.
* `HOP_UNIT_NOT_FOUND` — the unit does not exist on this box (renamed? removed?
  never installed?). Named, because silence here would hide a typo forever.
* `HOP_UNIT_UNREADABLE` — `systemctl show` returned nothing.
* `HOP_UNKNOWN_STATE` — an `ActiveState` this table does not know.
* `HOP_TRANSITION` — recorded in the state file, not alerted.

## The alert

One loud line on stdout (the unit appends it to
`~/.hermes/logs/hop-liveness-watch.log`) and one JSON record appended to
`~/.hermes/logs/hop-liveness-alerts.jsonl`:

```
ALERT hop=crier unit=crier-fleet.service state=down exit=0 kind=exited result=success down=41m since=2026-10-29T04:03:00Z reason=HOP_DOWN_CLEAN_EXIT
```

```json
{"schema":"hop-liveness/1","event":"down","hop":"crier","unit":"crier-fleet.service","state":"down","reason":"HOP_DOWN_CLEAN_EXIT","exit_code":"0","exit_kind":"exited","result":"success","down_s":2460,"down_for":"41m","since":"2026-10-29T04:03:00Z","at":"2026-10-29T04:44:00Z","repeat":false,"watchdog":"ops/hop-liveness-watch.sh"}
```

* `down_s` / `down_for` — how long the hop has been in its current (down) state.
  Derived from systemd's **monotonic** state-change stamp against `/proc/uptime`,
  so a clock or timezone change cannot fake it; falls back to the wall-clock stamp
  and reports `null` + `down=unknown` rather than asserting a number it does not
  have.
* `exit_code` / `exit_kind` — `ExecMainStatus` and what ended the process
  (`exited` / `killed` / `dumped` / `watchdog`).
* `repeat` — `false` on the transition, `true` on the hourly reminder while the
  hop stays down (an unread first alert must not decay into silence).
* A hop that answers again writes `event=recovered` and prints
  `RECOVERED hop=… was_down=…`.

Silent when every hop is up: no stdout, no alert record, exit 0. The watcher is
a **detector, not a repairer** — it never starts, stops or reconfigures a hop,
so it cannot fight an operator and cannot fail-loop.

## Install

```
install -m 0644 deploy/hop-liveness-watch.service deploy/hop-liveness-watch.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now hop-liveness-watch.timer
systemctl --user list-timers hop-liveness-watch.timer
```

The service executes the script **from the checkout**
(`~/coding-hermes-scheduler/coding-herms-scheduler/ops/hop-liveness-watch.sh`),
not from a deployed copy, so the watchdog and this document are the same file and
there is no copy to drift. Moving the checkout means editing the unit — a
deliberate, visible act.

## Falsifier (both directions)

The row's gate is that stopping a hop **with a clean exit** produces an alert
within one watchdog interval, while the pre-change configuration produces none.
Both directions are measured on this box, on the live `crier-fleet.service`:

```bash
# pre-change: no watcher installed
systemctl --user list-timers --all | grep -c hop-liveness        # 0
systemctl --user stop crier-fleet.service                        # clean stop
sleep 75; cat ~/.hermes/logs/hop-liveness-alerts.jsonl           # absent: nothing fires
systemctl --user start crier-fleet.service

# post-change: watcher installed at a one-minute interval
systemctl --user enable --now hop-liveness-watch.timer
systemctl --user stop crier-fleet.service                        # same clean stop
sleep 75; cat ~/.hermes/logs/hop-liveness-alerts.jsonl           # one HOP_DOWN_CLEAN_EXIT record
systemctl --user start crier-fleet.service
sleep 75; tail -1 ~/.hermes/logs/hop-liveness-alerts.jsonl       # event=recovered
```

Measured on 2026-10-29 (see the closed board row SCHED-GAP-16946 for the exact
output): pre-change window = 0 alerts; post-change window = 1 alert naming
`hop=crier unit=crier-fleet.service exit=… down=…`, then 1 recovery record.

The hermetic half lives in `ops/hop-liveness-watch-selftest.sh` (32 checks): it
drives the real watcher against a stub `systemctl` and separately proves the
selftest itself can fail (a mutated copy of the classifier is caught by the
"silent when clean" claim).

## What this does NOT cover

* **Answering is not liveness.** This proves a unit is *running*, not that the
  hop *answers*. `ops/cluster-chain-probe.sh` remains the on-demand check per
  hop, and its crier arm is unchanged by this watchdog — neither replaces the
  other.
* **Restart is not attempted.** AC1 of SCHED-GAP-16946 allows either
  restart-on-clean-exit or a watching timer; this is the watching timer. A hop
  that dies stays down until an operator or a repair leg acts.
* **This host only.** The timer runs on this control box. Nothing outside it
  watches this box, so a whole-host outage is invisible to this watchdog (the
  same limitation the box watcher has for boxes).
* **Delivery of the alert is a log write**, to
  `~/.hermes/logs/hop-liveness-watch.log` + `hop-liveness-alerts.jsonl`. No
  automated consumer reads those files yet — see SCHED-GAP-16967.
* **The connector hop** is report-only in the chain probe and is not a unit on
  this box, so it is not watched here.
