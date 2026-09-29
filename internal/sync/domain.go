package sync

import (
	"errors"
	"strings"
)

// DuckBrain memory domains (SCHED-GAP-1669).
//
// The DuckBrain HTTP API validates the `domain` of every write against a
// server-side enum (src/schema/memory.ts → DomainEnum, enforced in
// src/http/routes/memories.ts) and rejects anything else with
// `400 {"code":"VALIDATION_ERROR"}` before a row is stored.
//
// A rejected domain is a CLIENT defect, not a transport failure. Retrying it
// can never succeed, so it must never be spooled and must never mark the
// endpoint unreachable. Before this allowlist existed the sync sent
// domain="metrics" for /fleet/lane-output, which produced ~14.6K
// "Invalid domain 'metrics'" rejections, spooled a duplicate of the same write
// on every 5-minute cycle, and — because the rejection ran through the generic
// failure path — flipped sync health to "DuckBrain unreachable" with a HIGH
// alert each cycle. One bad domain at one call site must not look like an
// outage.

// duckbrainDomains is the allowlist of domain values DuckBrain accepts.
// Keep in lockstep with DomainEnum on the DuckBrain side.
var duckbrainDomains = []string{
	"person",
	"event",
	"concept",
	"message",
	"config",
	"raw_note",
}

// domainAliases maps a domain this codebase previously sent onto the accepted
// value that carries the same meaning. An entry here is a wire-compatibility
// shim for a value that was already in flight (spooled or in released code),
// NOT a licence to invent new local domain names: new call sites must pick a
// value from duckbrainDomains.
//
//	"metrics" → "config"
//
// /fleet/lane-output is an aggregate STATE snapshot posted on a cadence with
// change detection (canonicalPayloadHash strips the volatile synced_at) — the
// same shape as /fleet/summary and /fleet/namespaces, both of which already
// send "config". DuckBrain has no "metrics" domain.
var domainAliases = map[string]string{
	"metrics": "config",
}

// ErrDuckBrainDomainRejected marks a write this client refuses to send because
// its domain is not in duckbrainDomains and has no alias. Terminal and LOCAL:
// the caller skips the write (no spool, no health change), so a bad domain at
// one call site cannot masquerade as a transport outage.
var ErrDuckBrainDomainRejected = errors.New("duckbrain domain rejected")

// domainAllowed reports whether DuckBrain accepts d verbatim.
func domainAllowed(d string) bool {
	for _, ok := range duckbrainDomains {
		if d == ok {
			return true
		}
	}
	return false
}

// resolveDomain maps a requested domain onto the value to put on the wire.
//
//   - an accepted domain is returned unchanged
//   - a domain in domainAliases is remapped onto its accepted target
//   - anything else returns ("", false) and the caller MUST skip the write
//     rather than send a domain DuckBrain will reject
func resolveDomain(domain string) (string, bool) {
	if domainAllowed(domain) {
		return domain, true
	}
	if mapped, ok := domainAliases[domain]; ok {
		return mapped, true
	}
	return "", false
}

// allowedDomains returns the allowlist as a comma-separated string for logs.
func allowedDomains() string { return strings.Join(duckbrainDomains, ", ") }
