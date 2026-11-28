package blocks

// Approved destination-migration APPLY and ROLLBACK (SCHED-GAP-17084 rework).
//
// SCHED-GAP-17083 landed the policy store plus a non-mutating PREVIEW: an
// operator could see a "ready" plan item and had no way to act on it, and the
// only rollback was a hand restore of groups.jsonl. This file is the missing
// half — a reversible, idempotent, non-destructive migration path over the
// SAME store, with no second writer and no second source of truth:
//
//   - the project delivery target (projects.deliver) is written through the
//     caller's single writer for that column (internal/database.UpdateProject,
//     the same writer PUT /api/v1/projects uses) — never by touching SQLite
//     here;
//   - the change ledger lives in the group record in groups.jsonl, so it is
//     written by this package's one store writer (writeJSONL) and is read back
//     by a fresh process exactly like the policy is.
//
// Contract:
//
//   - NOT READY = NO WRITES. Every approval is re-validated against the
//     CURRENT store at apply time; one refused item refuses the whole batch
//     (all-or-nothing). A plan the operator saw yesterday cannot be applied
//     against a store that moved since.
//   - IDEMPOTENT. A second apply of the same approvals has nothing to move
//     (the lanes already equal the approved destination) and returns with
//     zero writes, so groups.jsonl stays byte-identical.
//   - NON-DESTRUCTIVE. The prior value of every moved lane is captured before
//     the write; nothing outside the moved set is touched, and a failed
//     project write is compensated back before returning.
//   - REVERSIBLE. Rollback restores the captured prior values (including an
//     unset lane) and appends its own ledger record. Rolling back twice, or
//     rolling back a group that never applied, is REFUSED — nothing captured.
//
// Destinations are never rendered by this file's callers: DestinationChange
// persists the raw prior/next pair (the same confidentiality class as the
// policy destination already stored in groups.jsonl) purely as the rollback
// capture; every API response, log line and evidence row goes through
// DestinationRef, i.e. <sha256:8>.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Destination ledger record kinds.
const (
	DestinationChangeApply    = "apply"
	DestinationChangeRollback = "rollback"
)

// Destination history kept per group. The ledger is a rollback capture plus an
// audit trail, not a full history: a bounded tail is enough to reverse the most
// recent migrations and keeps groups.jsonl small.
const maxDestinationHistory = 20

// Destination refusal codes (machine-readable, stable).
const (
	DestinationRefusedNotReady       = "not_ready"
	DestinationRefusedNothingCaptred = "nothing_captured"
	DestinationRefusedDigestMismatch = "digest_mismatch"
	DestinationRefusedStateDiverged  = "state_diverged"
)

// DestinationMove is one project delivery-target transition.
//
// Prior is the value captured immediately before the write; Next is the value
// the approved policy requires. Both are raw destinations: this struct IS the
// rollback capture. Render DestinationRef(Prior)/DestinationRef(Next) instead.
type DestinationMove struct {
	Project string `json:"project"`
	Prior   string `json:"prior,omitempty"`
	Next    string `json:"next"`
}

// DestinationChange is the persisted record of one apply or rollback: who did
// it, when, against which plan digest, and which lanes moved from where to
// where. It is appended to the group record in groups.jsonl.
type DestinationChange struct {
	Kind       string            `json:"kind"`
	Actor      string            `json:"actor"`
	At         string            `json:"at"`
	PlanDigest string            `json:"plan_digest"`
	Moves      []DestinationMove `json:"moves"`
}

// DestinationRef reduces one destination value to its 8-hex SHA-256
// fingerprint — enough to compare two runs or two nodes, not enough to
// reconstruct a chat id. Every operator-visible surface uses this.
func DestinationRef(destination string) string {
	if destination == "" {
		return ""
	}
	return DestinationFingerprint(destination)
}

// DestinationFingerprint is the 8-hex SHA-256 prefix of a destination value.
func DestinationFingerprint(destination string) string {
	sum := sha256.Sum256([]byte(destination))
	return hex.EncodeToString(sum[:])[:8]
}

// DestinationPolicyFingerprint digests a store's whole approved-mapping set
// (group names, approval flags, exception names and destinations) into
// sha256:<16 hex>. Two nodes whose groups.jsonl enforce different mappings
// therefore report different fingerprints, which is the observability half of
// the cross-node gap: there is no sync path, but divergence is no longer
// silent. No destination value is recoverable from the digest.
func DestinationPolicyFingerprint(groups []Group) string {
	lines := make([]string, 0, len(groups))
	for _, group := range groups {
		policy := group.DestinationPolicy
		if policy == nil || !policy.Approved {
			continue
		}
		line := group.Name + "=" + DestinationRef(policy.Destination)
		names := make([]string, 0, len(policy.Exceptions))
		for name := range policy.Exceptions {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			exception := policy.Exceptions[name]
			approval := "0"
			if exception.Approved {
				approval = "1"
			}
			line += ";" + name + ":" + approval + ":" + DestinationRef(exception.Destination)
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// destinationPlanDigest fingerprints an approved move set. It is derived from
// the sanitized refs only, so publishing it discloses nothing the refs do not.
func destinationPlanDigest(group string, moves []DestinationMove) string {
	lines := make([]string, 0, len(moves))
	for _, move := range moves {
		lines = append(lines, move.Project+"="+DestinationRef(move.Prior)+">"+DestinationRef(move.Next))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(group + "\n" + strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// DestinationChangeView is the operator-facing projection of a ledger record:
// the same who/when/plan-digest facts, with every destination reduced to a
// <sha256:8> reference. API responses and log lines serialize THIS, never
// DestinationChange.
type DestinationChangeView struct {
	Kind       string               `json:"kind"`
	Actor      string               `json:"actor"`
	At         string               `json:"at"`
	PlanDigest string               `json:"plan_digest"`
	Moves      []DestinationMoveRef `json:"moves"`
}

// DestinationMoveRef is one sanitized transition.
type DestinationMoveRef struct {
	Project  string `json:"project"`
	PriorRef string `json:"prior_ref,omitempty"`
	NextRef  string `json:"next_ref"`
}

// SanitizeDestinationChange projects a ledger record onto its public view.
func SanitizeDestinationChange(change DestinationChange) DestinationChangeView {
	moves := make([]DestinationMoveRef, 0, len(change.Moves))
	for _, move := range change.Moves {
		moves = append(moves, DestinationMoveRef{
			Project:  move.Project,
			PriorRef: DestinationRef(move.Prior),
			NextRef:  DestinationRef(move.Next),
		})
	}
	return DestinationChangeView{
		Kind:       change.Kind,
		Actor:      change.Actor,
		At:         change.At,
		PlanDigest: change.PlanDigest,
		Moves:      moves,
	}
}

// DestinationRefusal is a fail-closed refusal: nothing was written.
type DestinationRefusal struct {
	Code   string                `json:"code"`
	Reason string                `json:"reason"`
	Items  []DestinationPlanItem `json:"refused,omitempty"`
	Lanes  []string              `json:"lanes,omitempty"`
}

func (r *DestinationRefusal) Error() string {
	if len(r.Items) > 0 {
		return fmt.Sprintf("%s: %s (%d item(s) refused)", r.Code, r.Reason, len(r.Items))
	}
	return r.Code + ": " + r.Reason
}

// IsDestinationRefusal reports whether err is a fail-closed refusal (i.e. a
// caller error, not an IO fault).
func IsDestinationRefusal(err error) (*DestinationRefusal, bool) {
	var refusal *DestinationRefusal
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// DestinationApplyInput are the inputs of one approved migration.
type DestinationApplyInput struct {
	// Group is the owning group whose approved policy authorizes the moves.
	Group string
	// Approvals are the individually approved transitions (project, from, to).
	Approvals []DestinationApproval
	// Projects is the caller's current view of the fleet (name, namespace,
	// current delivery target). The engine never reads the database itself.
	Projects []DestinationProject
	// Actor is the authenticated operator identity, recorded in the ledger.
	Actor string
	// At is the clock reading for the ledger record (tests pin it).
	At time.Time
	// WriteTarget is the single writer for a project's delivery target.
	WriteTarget func(project, destination string) error
}

// DestinationApplyResult reports what an apply did.
type DestinationApplyResult struct {
	PlanDigest string
	Applied    int
	Already    int
	Moves      []DestinationMove
	Change     DestinationChange
}

// DestinationRollbackInput are the inputs of one rollback.
type DestinationRollbackInput struct {
	Group string
	// PlanDigest, when supplied, must match the captured apply — an operator
	// reversing a specific migration cannot accidentally reverse a newer one.
	PlanDigest  string
	Projects    []DestinationProject
	Actor       string
	At          time.Time
	WriteTarget func(project, destination string) error
}

// DestinationRollbackResult reports what a rollback restored.
type DestinationRollbackResult struct {
	PlanDigest string
	Restored   int
	Moves      []DestinationMove
	Change     DestinationChange
}

// ApplyDestinationMigration applies the ready subset of an approved plan.
//
// All-or-nothing: if any approval no longer matches the store, nothing is
// written and a *DestinationRefusal (code not_ready) is returned carrying the
// refused plan items. When every approved lane already equals its target the
// call writes nothing at all and reports Already, which is what makes a repeat
// apply byte-identical.
func (s *Store) ApplyDestinationMigration(in DestinationApplyInput) (DestinationApplyResult, error) {
	if in.WriteTarget == nil {
		return DestinationApplyResult{}, errors.New("apply destination migration: no target writer supplied")
	}
	if len(in.Approvals) == 0 {
		return DestinationApplyResult{}, &DestinationRefusal{
			Code:   DestinationRefusedNotReady,
			Reason: "no approvals supplied — nothing is authorized to move",
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	groups, err := loadJSONL(s.groupsPath, func(g *Group) string { return g.Name })
	if err != nil {
		return DestinationApplyResult{}, err
	}
	index, group, ok := findGroup(groups, in.Group)
	if !ok {
		return DestinationApplyResult{}, fmt.Errorf("group %q: %w", in.Group, ErrNotFound)
	}
	if group.DestinationPolicy == nil || !group.DestinationPolicy.Approved {
		return DestinationApplyResult{}, &DestinationRefusal{
			Code:   DestinationRefusedNotReady,
			Reason: "group " + in.Group + " has no APPROVED destination policy — there is no authority to migrate to",
		}
	}

	current := projectIndex(in.Projects)
	plan := PlanDestinationMigration(groups, approvedPolicies(groups), planTargets(group, current), in.Approvals)
	byProject := make(map[string]DestinationPlanItem, len(plan))
	for _, item := range plan {
		byProject[item.Project] = item
	}

	var moves []DestinationMove
	var refused []DestinationPlanItem
	already := 0
	seen := make(map[string]bool, len(in.Approvals))
	for _, approval := range in.Approvals {
		if seen[approval.Project] {
			refused = append(refused, DestinationPlanItem{Project: approval.Project, Status: "ambiguous"})
			continue
		}
		seen[approval.Project] = true
		item, exists := byProject[approval.Project]
		if !exists {
			refused = append(refused, DestinationPlanItem{Project: approval.Project, Status: "not_a_member"})
			continue
		}
		switch item.Status {
		case "ready":
			moves = append(moves, DestinationMove{
				Project: approval.Project,
				Prior:   current[approval.Project].Destination,
				Next:    approval.To,
			})
		case "aligned":
			// The lane already carries the approved target. Only an approval
			// that names that same target is a redundant apply; anything else
			// is a plan that no longer matches the store.
			if approval.To == current[approval.Project].Destination {
				already++
				continue
			}
			refused = append(refused, DestinationPlanItem{Project: approval.Project, Status: "stale_approval"})
		default:
			refused = append(refused, DestinationPlanItem{Project: approval.Project, Status: item.Status})
		}
	}
	if len(refused) > 0 {
		return DestinationApplyResult{}, &DestinationRefusal{
			Code:   DestinationRefusedNotReady,
			Reason: "the approved plan no longer matches the store — re-run the preview and approve again",
			Items:  refused,
		}
	}

	result := DestinationApplyResult{Already: already, PlanDigest: destinationPlanDigest(group.Name, moves)}
	if len(moves) == 0 {
		// Nothing to move: no group write, no project write. A repeat apply is
		// therefore byte-identical to the first one.
		return result, nil
	}

	writes, err := applyMoves(moves, in.WriteTarget)
	if err != nil {
		return DestinationApplyResult{}, err
	}
	change := DestinationChange{
		Kind:       DestinationChangeApply,
		Actor:      in.Actor,
		At:         in.At.UTC().Format(time.RFC3339),
		PlanDigest: result.PlanDigest,
		Moves:      moves,
	}
	groups[index].DestinationHistory = appendDestinationHistory(groups[index].DestinationHistory, change)
	if err := writeJSONL(s.groupsPath, groups); err != nil {
		// The ledger is the only thing that makes this reversible: if it
		// cannot be recorded, put the lanes back and fail.
		restoreErr := restoreMoves(writes, in.WriteTarget)
		if restoreErr != nil {
			return DestinationApplyResult{}, fmt.Errorf("record migration ledger: %w (compensating restore also failed: %v)", err, restoreErr)
		}
		return DestinationApplyResult{}, fmt.Errorf("record migration ledger: %w (all lanes restored)", err)
	}
	result.Applied = len(moves)
	result.Moves = moves
	result.Change = change
	return result, nil
}

// RollbackDestinationMigration restores the prior values captured by the most
// recent apply for the group.
func (s *Store) RollbackDestinationMigration(in DestinationRollbackInput) (DestinationRollbackResult, error) {
	if in.WriteTarget == nil {
		return DestinationRollbackResult{}, errors.New("rollback destination migration: no target writer supplied")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	groups, err := loadJSONL(s.groupsPath, func(g *Group) string { return g.Name })
	if err != nil {
		return DestinationRollbackResult{}, err
	}
	index, group, ok := findGroup(groups, in.Group)
	if !ok {
		return DestinationRollbackResult{}, fmt.Errorf("group %q: %w", in.Group, ErrNotFound)
	}

	captured := -1
	for i := len(group.DestinationHistory) - 1; i >= 0; i-- {
		switch group.DestinationHistory[i].Kind {
		case DestinationChangeRollback:
			// The ledger is a stack: the most recent change is already a
			// rollback, so there is nothing left to reverse.
			return DestinationRollbackResult{}, &DestinationRefusal{
				Code:   DestinationRefusedNothingCaptred,
				Reason: "the most recent change for group " + in.Group + " is itself a rollback — nothing is captured to reverse",
			}
		case DestinationChangeApply:
			captured = i
		}
		if captured >= 0 {
			break
		}
	}
	if captured < 0 {
		return DestinationRollbackResult{}, &DestinationRefusal{
			Code:   DestinationRefusedNothingCaptred,
			Reason: "no applied destination migration is captured for group " + in.Group + " — nothing to roll back",
		}
	}
	record := group.DestinationHistory[captured]
	if in.PlanDigest != "" && in.PlanDigest != record.PlanDigest {
		return DestinationRollbackResult{}, &DestinationRefusal{
			Code:   DestinationRefusedDigestMismatch,
			Reason: "plan digest mismatch: the captured migration is " + record.PlanDigest,
		}
	}

	current := projectIndex(in.Projects)
	var diverged []string
	for _, move := range record.Moves {
		if current[move.Project].Destination != move.Next {
			diverged = append(diverged, move.Project)
		}
	}
	if len(diverged) > 0 {
		sort.Strings(diverged)
		return DestinationRollbackResult{}, &DestinationRefusal{
			Code:   DestinationRefusedStateDiverged,
			Reason: "lane target changed after the migration was applied — rollback would clobber a newer decision; restore by hand or apply a fresh approved plan",
			Lanes:  diverged,
		}
	}

	moves := make([]DestinationMove, 0, len(record.Moves))
	for _, move := range record.Moves {
		moves = append(moves, DestinationMove{Project: move.Project, Prior: move.Next, Next: move.Prior})
	}
	writes, err := applyMoves(moves, in.WriteTarget)
	if err != nil {
		return DestinationRollbackResult{}, err
	}
	change := DestinationChange{
		Kind:       DestinationChangeRollback,
		Actor:      in.Actor,
		At:         in.At.UTC().Format(time.RFC3339),
		PlanDigest: record.PlanDigest,
		Moves:      moves,
	}
	groups[index].DestinationHistory = appendDestinationHistory(groups[index].DestinationHistory, change)
	if err := writeJSONL(s.groupsPath, groups); err != nil {
		restoreErr := restoreMoves(writes, in.WriteTarget)
		if restoreErr != nil {
			return DestinationRollbackResult{}, fmt.Errorf("record rollback ledger: %w (compensating restore also failed: %v)", err, restoreErr)
		}
		return DestinationRollbackResult{}, fmt.Errorf("record rollback ledger: %w (all lanes restored)", err)
	}
	return DestinationRollbackResult{
		PlanDigest: record.PlanDigest,
		Restored:   len(moves),
		Moves:      moves,
		Change:     change,
	}, nil
}

// applyMoves writes each move and returns the ones already written, so a
// mid-batch failure can be compensated. It never leaves a partial batch behind
// on its own: the caller restores on error.
func applyMoves(moves []DestinationMove, write func(project, destination string) error) ([]DestinationMove, error) {
	written := make([]DestinationMove, 0, len(moves))
	for _, move := range moves {
		if err := write(move.Project, move.Next); err != nil {
			restoreErr := restoreMoves(written, write)
			if restoreErr != nil {
				return nil, fmt.Errorf("write %s: %w (compensating restore also failed: %v)", move.Project, err, restoreErr)
			}
			return nil, fmt.Errorf("write %s: %w (all %d written lane(s) restored)", move.Project, err, len(written))
		}
		written = append(written, move)
	}
	return written, nil
}

// restoreMoves puts every already-written lane back to its captured prior
// value, newest first so a repeated project lands on its original value.
func restoreMoves(written []DestinationMove, write func(project, destination string) error) error {
	var firstErr error
	for i := len(written) - 1; i >= 0; i-- {
		if err := write(written[i].Project, written[i].Prior); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("restore %s: %w", written[i].Project, err)
		}
	}
	return firstErr
}

// appendDestinationHistory appends one record and keeps the newest
// maxDestinationHistory entries.
func appendDestinationHistory(history []DestinationChange, change DestinationChange) []DestinationChange {
	history = append(history, change)
	if len(history) > maxDestinationHistory {
		history = history[len(history)-maxDestinationHistory:]
	}
	return history
}

func findGroup(groups []Group, name string) (int, Group, bool) {
	for i := range groups {
		if groups[i].Name == name {
			return i, groups[i], true
		}
	}
	return -1, Group{}, false
}

func approvedPolicies(groups []Group) map[string]DestinationPolicy {
	policies := make(map[string]DestinationPolicy, len(groups))
	for _, group := range groups {
		if group.DestinationPolicy != nil {
			policies[group.Name] = *group.DestinationPolicy
		}
	}
	return policies
}

func projectIndex(projects []DestinationProject) map[string]DestinationProject {
	index := make(map[string]DestinationProject, len(projects))
	for _, project := range projects {
		index[project.Name] = project
	}
	return index
}

// planTargets returns the group's members as plan targets, using the caller's
// current view. A member missing from the view is still planned (with an empty
// destination) so the plan can report it rather than silently dropping it.
func planTargets(group Group, current map[string]DestinationProject) []DestinationProject {
	targets := make([]DestinationProject, 0, len(group.Projects))
	for _, name := range group.Projects {
		project, ok := current[name]
		if !ok {
			project = DestinationProject{Name: name}
		}
		targets = append(targets, project)
	}
	return targets
}
