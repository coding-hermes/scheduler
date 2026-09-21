#!/bin/sh
# GAP-060 test stub for fleet-cooldown-policy.py — exits 1 (simulated policy
# failure; callers must log-and-proceed, never poison the API response).
# Selected via SCHEDULER_POLICY_SCRIPT (documented test override in
# internal/api/server_projects.go policyScriptPath).
echo "gap060 stub: simulated policy failure" >&2
exit 1
