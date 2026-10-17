package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// digestPattern is the closed sha256 digest shape every canonical digest in
// this package produces and consumes.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ReadyStepIntent is the closed foreman-provided record that permits exactly
// one just-in-time child attempt. It is presented only when the step is ready:
// planned and blocked steps are foreman state and never reach the scheduler.
//
// BriefDigest is the canonical execution-brief digest of the closed brief this
// step executes under (the same digest the foreman carries into Task Router
// and the A2A task); ContextRefs are its sourced context references.
//
// It carries no credentials and no hidden chain-of-thought — only identities,
// receipts, declarations and policy.
type ReadyStepIntent struct {
	ParentRunID          string                  `json:"parent_run_id"`
	StepID               string                  `json:"step_id"`
	StepGeneration       int                     `json:"step_generation"`
	BriefDigest          string                  `json:"brief_digest"`
	ContextRefs          []ContextRef            `json:"context_refs"`
	RequiredCapabilities []string                `json:"required_capabilities"`
	Route                RouteReceipt            `json:"route"`
	Choice               ToolChoice              `json:"choice"`
	Lease                WorkspaceLeaseAuthority `json:"lease"`
	Policy               AttemptPolicy           `json:"policy"`
}

// RouteReceipt is the just-in-time Task Router decision: ordered model/
// provider candidates plus route evidence and a short decision expiry. The
// scheduler never reorders candidates and never re-resolves a stale receipt.
type RouteReceipt struct {
	ReceiptDigest string           `json:"receipt_digest"`
	Candidates    []RouteCandidate `json:"candidates"`
	ResolvedAt    string           `json:"resolved_at"`
	ValidUntil    string           `json:"valid_until"`
}

// RouteCandidate is one ranked model/provider candidate. ExclusionEvidence is
// the explicit, machine-readable evidence that lets a later candidate be
// chosen; without it the head candidate must be chosen.
type RouteCandidate struct {
	Rank              int    `json:"rank"`
	Model             string `json:"model"`
	Provider          string `json:"provider"`
	BaseURL           string `json:"base_url"`
	ExclusionEvidence string `json:"exclusion_evidence"`
}

// ToolChoice is the foreman's compatible tool/connector selection inside the
// chosen candidate. Connector ranking never reorders Task Router candidates.
type ToolChoice struct {
	CandidateRank        int      `json:"candidate_rank"`
	ConnectorID          string   `json:"connector_id"`
	ConnectorVersion     string   `json:"connector_version"`
	CompatibleModel      string   `json:"compatible_model"`
	CompatibleProvider   string   `json:"compatible_provider"`
	DeclaredCapabilities []string `json:"declared_capabilities"`
}

// WorkspaceLeaseAuthority is the verified Bunker workspace lease/profile
// authority the attempt must execute under. Presence and currency are checked
// fail-closed before any attempt row exists.
type WorkspaceLeaseAuthority struct {
	LeaseID               string `json:"lease_id"`
	LeaseEpoch            int    `json:"lease_epoch"`
	ProfileID             string `json:"profile_id"`
	EnforcementReceiptRef string `json:"enforcement_receipt_ref"`
	VerifiedAt            string `json:"verified_at"`
	ValidUntil            string `json:"valid_until"`
}

// AttemptPolicy is the timeout/budget/cancellation policy carried with the
// attempt. StartTimeoutS is the deadline for native start evidence — when it
// passes, the start becomes uncertain and reconciliation, never a retry,
// follows. Budgets are named integer units.
type AttemptPolicy struct {
	StartTimeoutS      int64  `json:"start_timeout_s"`
	WallTimeoutS       int64  `json:"wall_timeout_s"`
	BudgetUnits        int64  `json:"budget_units"`
	CancellationPolicy string `json:"cancellation_policy"`
}

// ValidateReadyStepIntent structurally validates one intent fail-closed. Time
// currency (route expiry, lease freshness) is checked by the admission gate
// against an explicit now.
func ValidateReadyStepIntent(in ReadyStepIntent) error {
	if in.StepID == "" && in.StepGeneration == 0 && in.BriefDigest == "" {
		return ErrNoReadyStepIntent
	}
	if in.ParentRunID == "" || in.StepID == "" {
		return fmt.Errorf("%w: missing parent/step identity", ErrReadyStepIntent)
	}
	if !digestPattern.MatchString(in.BriefDigest) {
		return fmt.Errorf("%w: canonical execution-brief digest is not a sha256 digest", ErrReadyStepIntent)
	}
	if in.StepGeneration < 1 {
		return fmt.Errorf("%w: step generation must be >= 1", ErrReadyStepIntent)
	}
	if in.Policy.StartTimeoutS < 1 || in.Policy.WallTimeoutS < 1 || in.Policy.BudgetUnits < 0 || in.Policy.CancellationPolicy == "" {
		return fmt.Errorf("%w: timeout/budget/cancellation policy incomplete", ErrReadyStepIntent)
	}
	if in.Lease.LeaseID == "" || in.Lease.LeaseEpoch < 1 || in.Lease.ProfileID == "" || in.Lease.EnforcementReceiptRef == "" {
		return ErrLeaseAuthorityMissing
	}
	if _, err := time.Parse(time.RFC3339Nano, in.Lease.VerifiedAt); err != nil {
		return fmt.Errorf("%w: lease verified_at: %v", ErrLeaseAuthorityMissing, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, in.Lease.ValidUntil); err != nil {
		return fmt.Errorf("%w: lease valid_until: %v", ErrLeaseAuthorityMissing, err)
	}
	return validateRouteReceiptShape(in.Route)
}

func validateRouteReceiptShape(r RouteReceipt) error {
	if r.ReceiptDigest == "" {
		return ErrRouteReceiptMissing
	}
	if len(r.Candidates) == 0 {
		return fmt.Errorf("%w: no candidates", ErrRouteReceiptInvalid)
	}
	for i, c := range r.Candidates {
		if c.Rank != i+1 {
			return fmt.Errorf("%w: candidate ranks must be 1..n in order", ErrCandidateOrder)
		}
		if c.Model == "" || c.Provider == "" {
			return fmt.Errorf("%w: candidate %d missing model/provider", ErrRouteReceiptInvalid, c.Rank)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, r.ResolvedAt); err != nil {
		return fmt.Errorf("%w: resolved_at: %v", ErrRouteReceiptInvalid, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.ValidUntil); err != nil {
		return fmt.Errorf("%w: valid_until: %v", ErrRouteReceiptInvalid, err)
	}
	return nil
}

// RouteReceiptDigest returns the sha256-prefixed digest of the receipt over
// its JCS-canonical form with receipt_digest omitted — the same construction
// as BriefDigest. Task Router computes the digest it returns over exactly
// this body.
func RouteReceiptDigest(r RouteReceipt) (string, error) {
	body := r
	body.ReceiptDigest = ""
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal route receipt: %w", err)
	}
	canonical, err := canonicalBytesOmitting(raw, "receipt_digest")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyRouteReceiptDigest recomputes the receipt digest and compares it to
// the claimed digest. Tampered bodies are refused.
func VerifyRouteReceiptDigest(r RouteReceipt) error {
	want, err := RouteReceiptDigest(r)
	if err != nil {
		return err
	}
	if want != r.ReceiptDigest {
		return fmt.Errorf("%w: calculated %s", ErrRouteReceiptTampered, want)
	}
	return nil
}

// EvidenceDigest returns a deterministic sha256-prefixed digest over the given
// fields. It is the append-only evidence idempotency key: identical evidence
// re-presents the same digest and never appends twice.
func EvidenceDigest(fields ...string) string {
	h := sha256.New()
	h.Write([]byte("execution-evidence-v1"))
	for _, f := range fields {
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// CapabilitySet returns a sorted copy of names with duplicates removed.
func CapabilitySet(names []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// DeriveAttemptIdempotencyKey derives the replay identity of one attempt from
// (parent run, step, generation, route digest, workspace lease epoch). It is
// never minted from a random id and never names an unrelated pending task: a
// retry with a new generation derives a new key, and a replay of the same
// preparation derives the same key.
func DeriveAttemptIdempotencyKey(parentRunID, stepID string, generation int, routeDigest string, leaseEpoch int) string {
	h := sha256.New()
	h.Write([]byte("execution-attempt-key-v1"))
	for _, f := range []string{parentRunID, stepID, fmt.Sprintf("%d", generation), routeDigest, fmt.Sprintf("%d", leaseEpoch)} {
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return "aik-1:" + hex.EncodeToString(h.Sum(nil))
}
