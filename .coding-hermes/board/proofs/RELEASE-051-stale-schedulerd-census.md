# RELEASE-051 — stale /usr/local/bin/schedulerd: census and blocked removal

Tick: coding-hermes-scheduler-foreman-2026-11-07-23-12-40
Status: BLOCKED on owner consent (root-owned destructive removal). Everything measurable
was measured; the single remaining act needs root and was gate-blocked unattended.

## ARM 3 (complete) — the whole host is swept, every binary classified

```
$ ls -la /usr/local/bin/scheduler* /usr/bin/scheduler* ~/.local/bin/scheduler* \
        /home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/scheduler*
ls: cannot access '/usr/bin/scheduler*': No such file or directory
ls: cannot access '/home/kara/.local/bin/scheduler*': No such file or directory
-rwxrwxr-x 1 kara kara 18665737 Nov  7 00:42 .../coding-herms-scheduler/bin/schedulerd
-rwxr-xr-x 1 root root 20266468 Jul 19 09:50 /usr/local/bin/schedulerd
```

| path | owner | size | buildinfo |
|---|---|---|---|
| /usr/local/bin/schedulerd | root:root | 20,266,468 | mod `github.com/coding-herms/scheduler v1.0.1-0.20260719145003-c56721124655+dirty`, pre-rename module path, CGO_ENABLED=1, NO vcs.revision / vcs.time |
| .../coding-herms-scheduler/bin/schedulerd | kara:kara | 18,665,737 | mod `github.com/coding-hermes/scheduler v1.14.1-0.20261107042958-d0dad6d9ede0+dirty` |

Stale binary identity recorded before any removal attempt:

```
$ sha256sum /usr/local/bin/schedulerd
09975492f425444609ccffd766ddc7a47ce860ab80f95a00176acca53a0f6797  /usr/local/bin/schedulerd
$ stat -c '%n size=%s owner=%U:%G mtime=%y' /usr/local/bin/schedulerd
/usr/local/bin/schedulerd size=20266468 owner=root:root mtime=2026-07-19 09:50:33
```

Live daemon surfaces (both ExecStart the repo bin path, neither uses /usr/local/bin):

```
$ grep -h '^ExecStart=' ~/.config/systemd/user/coding-hermes-scheduler.service \
      ~/.config/systemd/user/schedulerd-test-canary.service
ExecStart=/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd \
ExecStart=/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd \
```

Newest tag at census time: `v1.15.2` (a sibling releng lane cut it during this tick;
`git tag --sort=-v:refname | head -1` reported v1.15.1 earlier in the same tick).

## Reference sweep (precondition for deleting)

```
$ grep -rl 'usr/local/bin/schedulerd' ~/.hermes/scripts/ \
        /home/kara/coding-hermes-scheduler/coding-herms-scheduler/scripts/ \
        /etc/systemd/ ~/.config/systemd/ /etc/cron.d/ /etc/crontab
   (no output)
$ crontab -l | grep -c schedulerd
0
```

Nothing references the stale path: no unit, script, or crontab entry invokes it, and no unit
uses the bare `schedulerd` name.

## ARM 1 / ARM 2 — NOT satisfied, and why

The stale binary is still present, so `command -v schedulerd` still resolves to it and the
ARM 1/ARM 2 conditions cannot hold yet. The removal was attempted as

```
$ sudo rm -f /usr/local/bin/schedulerd
BLOCKED: Command timed out without user response. The user has NOT consented to this
action. ... Silence is not consent.
```

`sudo -n true` succeeds for this account, so the block is the harness destructive-action
consent gate, not a privilege failure. Sidestepping a blocked destructive action is
forbidden, so the binary was left in place and the row is handed to the owner.

## The fix, when authorized (one command)

```
sudo rm -f /usr/local/bin/schedulerd
```

Then ARM 3 requires exactly one remaining entry — the live unit ExecStart path — which is
already true, and `command -v schedulerd` must exit non-zero.

A symlink pointing at the repo bin would NOT satisfy ARM 1: that binary reports
`v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, not the newest tag `v1.15.2`, so the
"symlink to the canonical unit ExecStart path" branch of ARM 2 would be the only route and
it still leaves a binary whose module version is behind the newest tag.
