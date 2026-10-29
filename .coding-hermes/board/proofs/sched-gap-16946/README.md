# SCHED-GAP-16946 — hop liveness watchdog, live falsifier evidence

Measured on 2026-10-29 on the control box (karaHermes-mde-7840hs), against the
live `crier-fleet.service` and the code merged as `eb35801a` + `49fda479`
(origin/main at the time of the run; the board closeout commit follows).

| file | what it is |
| --- | --- |
| `prechange-control.log` | **pre-change direction**: no watchdog timer installed (0 hop-liveness timers, alert file absent); `systemctl --user stop crier-fleet.service` → `ActiveState=inactive SubState=dead Result=success ExecMainCode=1 ExecMainStatus=0`; 75s later the alert file and the watcher log are still absent → the pre-change configuration produces no alert. |
| `postchange-alert.log` | **post-change direction**: timer enabled; the same clean stop at 18:56:26Z produced `ALERT hop=crier unit=crier-fleet.service state=down exit=0 kind=exited result=success down=53s since=2026-10-29T18:56:26Z reason=HOP_DOWN_CLEAN_EXIT` at the 18:57:19Z tick = 53s, inside one 60s interval. |
| `recovery.log` | the hop restarted at 18:57:25Z; the 18:58:19Z tick wrote `event=recovered` (`RECOVERED hop=crier … was_down=53s`), and `ops/cluster-chain-probe.sh crier` answered PASS (`health=ok version=0.2.0-215-gca27c855 …`). |
| `alerts-copy.jsonl` | the two records the watchdog appended during the test (down + recovered), verbatim. |
| `selftest.log` | `ops/hop-liveness-watch-selftest.sh` — 32 checks, 32 passed, including the mutation control that turns the selftest red when the up/down classification is flipped. |

Reproduce with the commands in `docs/hop-liveness-watchdog.md` § Falsifier. Note
that the live direction genuinely stops `crier-fleet` for about a minute; run it
when the bus is not carrying a dispatch, and start the hop again immediately.
