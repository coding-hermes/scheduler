#!/bin/sh
# GAP-060 test stub for fleet-cooldown-policy.py — exits 0, records nothing.
# Selected via SCHEDULER_POLICY_SCRIPT (documented test override in
# internal/api/server_projects.go policyScriptPath) so unit tests never run
# the real ops script against the live ~/.hermes/fleet.toml.
exit 0
