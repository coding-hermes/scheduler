# deploy/ — systemd units and deployment helpers

Fleet scheduler service units live here (`coding-hermes-scheduler.service`,
`coding-hermes-scheduler-gateway.service`, `watchdog.*`). See `gateway-setup.md`
for the gateway key setup.

## fleet-sync: DB -> fleet.toml mirror (SCHED-GAP-211-WIRE)

`~/.hermes/scripts/fleet-sync.py` (canonical location, outside this repo) is the
sanctioned mirror of scheduler DB state into `~/.hermes/fleet.toml`. It is
one-directional: it READS the scheduler DB and WRITES the TOML file — it never
PUTs anything back to the API. Default is dry-run; `--write` makes it write the
file atomically.

Without wiring, the mirror only runs when someone remembers. The units here make
it a durable daily job (systemd **user** units — no root needed):

- `fleet-sync.service` — oneshot that runs the script with `--write`
  (`ExecStart=/usr/bin/env python3 %h/.hermes/scripts/fleet-sync.py --write`),
  logging to `~/.hermes/logs/fleet-sync.log` (60s timeout).
- `fleet-sync.timer` — fires daily at 06:30 local time with a 0-300s random
  delay; `Persistent=true` catches missed runs after downtime.

### Enable

```sh
mkdir -p ~/.config/systemd/user
cp deploy/fleet-sync.service deploy/fleet-sync.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now fleet-sync.timer
```

### Verify

```sh
systemctl --user list-timers fleet-sync.timer
systemctl --user status fleet-sync.service
stat ~/.hermes/fleet.toml          # mtime advances after each run
tail ~/.hermes/logs/fleet-sync.log
```
