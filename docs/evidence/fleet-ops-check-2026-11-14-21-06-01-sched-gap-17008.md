# Fleet ops measurement: SCHED-GAP-17008

Tick: fleet-ops-check-2026-11-14-21-06-01
Measured: 2026-11-14T16:12:38-05:00
Repository evidence was captured from the live scheduler host. Home-directory prefixes are rendered as `~` in this committed artifact.

## Published board and recovery gate

`git show origin/main:.coding-hermes/waves/coding-hermes-scheduler-foreman-2026-11-14-09-03-05.json | jq '{finished_at,recovery_status}'`

Output:

`{"finished_at":"2026-11-14T16:25:00Z","recovery_status":"recovery_complete_no_dispatch"}`

The manifest's two listed implementation commits were already ancestors of `origin/main`; both listed worker branches and worktree paths were absent. The recovery barrier was therefore terminal before the normal ops measurement.

## Live ops row measurement

Command:

`sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db -header -column "SELECT p.name, COUNT(t.id) AS completed_72h FROM projects p LEFT JOIN ticks t ON t.project_name=p.name AND t.status='completed' AND julianday(t.created_at) >= julianday('now','-72 hours') WHERE p.name LIKE '%-ops' AND p.enabled=1 GROUP BY p.name ORDER BY p.name;"`

Output:

```text
name                              completed_72h
boardctl-ops                      1
bunker-ops                        3
code-factory-ops                  1
coding-hermes-scheduler-ops       1
consensus-ops                     1
crier-ops                         2
duckbrain-ops                     1
gitreins-ops                      1
heading-ops                       2
muster-ops                        1
task-router-ops                   2
terminal-jail-ops                1
warpfs-ops                        1
wojons-mythos-ops                 3
```

Summary command:

`sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db -header -column "WITH ops AS (SELECT p.name FROM projects p WHERE p.name LIKE '%-ops' AND p.enabled=1), counts AS (SELECT o.name, COUNT(t.id) AS completed FROM ops o LEFT JOIN ticks t ON t.project_name=o.name AND t.status='completed' AND julianday(t.created_at) >= julianday('now','-72 hours') GROUP BY o.name) SELECT COUNT(*) AS enabled_ops, SUM(CASE WHEN completed>0 THEN 1 ELSE 0 END) AS with_completed_72h, SUM(CASE WHEN completed=0 THEN 1 ELSE 0 END) AS without_completed_72h, GROUP_CONCAT(CASE WHEN completed=0 THEN name END) AS missing FROM counts;"`

Output:

`enabled_ops=14 with_completed_72h=14 without_completed_72h=0 missing=NULL`

This refutes the original 10-of-11-zero-completion snapshot for the current enabled fleet. It does not close the row because the remaining admission-reason and repository verification criteria were not proven in this ops tick.

## Failure classification

Command:

`sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db -header -column "SELECT COALESCE(NULLIF(failure_reason,''),'(empty)') AS failure_reason, COALESCE(NULLIF(admit_reason,''),'(empty)') AS admit_reason, COALESCE(NULLIF(dispatch_reason,''),'(empty)') AS dispatch_reason, COUNT(*) AS n FROM ticks WHERE project_name LIKE '%-ops' AND julianday(created_at) >= julianday('now','-72 hours') AND status='failed' GROUP BY 1,2,3 ORDER BY n DESC;"`

Output:

```text
failure_reason  admit_reason        dispatch_reason  n
gateway_5xx     ok                  unavailable      14
gateway_5xx     resume:board_wake   unavailable       3
```

The live failed rows are recorded as generic `gateway_5xx` with `dispatch_reason=unavailable`; this tick did not prove the row's required 429-versus-503 admission classification. No mutation or restart was performed.

## Spawn histogram and service liveness

Command:

`sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db -header -column "WITH m AS (SELECT strftime('%Y-%m-%d %H:%M', created_at) AS minute, COUNT(*) AS n FROM ticks WHERE project_name LIKE '%-ops' AND julianday(created_at) >= julianday('now','-24 hours') GROUP BY minute) SELECT MAX(n) AS max_ops_spawns_per_minute, SUM(CASE WHEN n>=5 THEN 1 ELSE 0 END) AS minutes_at_or_above_5, COUNT(*) AS active_minutes FROM m;"`

Output:

`max_ops_spawns_per_minute=1 minutes_at_or_above_5=0 active_minutes=11`

Command:

`systemctl --user show coding-hermes-scheduler.service -p MainPID -p ActiveEnterTimestamp -p NRestarts`

Output:

```text
ActiveEnterTimestamp=Wed 2026-11-11 17:20:10 -05
MainPID=276029
NRestarts=0
```

Command:

`ss -ltnp | grep -E ':(9090)\\b'`

Output:

`LISTEN 0 4096 127.0.0.1:9090 0.0.0.0:* users:(("schedulerd",pid=276029,fd=16))`

The service remained live with a stable PID and zero restarts. No restart was attempted because no fault requiring restart was established and the current tick census contained in-flight work elsewhere.

## Scope and closeout

This was measurement-only. No scheduler database, lane configuration, service unit, or project code was changed. The row remains pending: current completion coverage and the 24-hour spawn collision arm measure clean, while the required specific admission-reason persistence and repository test arm remain unverified. Evidence digest is computed after the final scan.
