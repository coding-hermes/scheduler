package api

// SCHED-GAP-1765 — the family onboarding template.
//
// The scheduler API grows lanes one at a time: POST /api/v1/projects writes
// exactly the row in its body. Nothing in this repository ever minted the
// eleven-lane family (foreman + all nine role suffixes + a duplicate) that the
// live fleet carries — that template lives in host-side provisioners
// (~/.hermes/scripts/complete_satellite_families.py,
// satellite-coverage-reconcile.py). This file is the in-repo half of the
// owner's 2026-11-16 ruling (704 lanes / 118 families, 445 enabled support
// lanes against 49 enabled foremen — 9:1 support against the 10%
// support-capacity doctrine):
//
//  1. DECLARE the smaller default set — the scheduler package owns it
//     (scheduler.DefaultFamilyRoles: -qa -sync -docs; the foreman is the
//     project row itself, so a default family is four lanes).
//  2. ENFORCE it on the onboarding path — a create body that asks for a family
//     (`"family": true`) mints exactly those satellites, all of them born
//     DISARMED (SCHED-GAP-1729) and each one validated by the same
//     SCHED-GAP-138 shape gate a hand-written create hits.
//  3. KEEP the on-demand path — a plain single-lane create stays exactly as it
//     was, so any of the six on-demand suffixes (-pm -dogfood -perf -releng
//     -review -readme) can still be minted deliberately, forever.
//
// Nothing here reads, rewrites or deletes an existing project row. A satellite
// that already exists is reported as `existing` and left byte-identical: the
// mint is additive and idempotent, which is what makes it safe to re-run over
// a fleet that is already provisioned.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// familyCreateProbe is the extra create-body field the family template reads.
//
// It is decoded SEPARATELY from the Project: database.Project implements
// json.UnmarshalJSON on a pointer receiver, so embedding it in a request
// struct would promote that method to the outer type and make the decoder hand
// the WHOLE object to Project.UnmarshalJSON — silently dropping this flag.
// Two decodes over one buffered body is the shape that cannot hit that trap.
type familyCreateProbe struct {
	Family bool `json:"family"`
}

// satelliteFamilyNamespaces maps a role suffix onto the namespace its family
// lives in (live convention, mirrored from the host provisioners' NS / FAM_NS
// tables so an in-repo mint lands where the scheduler already expects the
// family). Only the four default-family roles are used by the onboarding mint;
// the remaining entries are carried so the table stays total over
// scheduler.LaneRoleSuffixes() and a future on-demand mint can reuse it.
var satelliteFamilyNamespaces = map[string]string{
	"qa":      "qa",
	"pm":      "pm",
	"sync":    "duckbrain-sync",
	"dogfood": "dogfood",
	"releng":  "releases",
	"docs":    "docs",
	"readme":  "docs",
	"review":  "review",
	"perf":    "perf",
}

// familyMintResult is what one onboarding mint produced.
type familyMintResult struct {
	// DefaultRoles is the declared template (scheduler.DefaultFamilyRoles).
	DefaultRoles []string `json:"default_roles"`
	// OnDemandRoles are the roles this request deliberately does NOT mint.
	OnDemandRoles []string `json:"on_demand_roles"`
	// Created names the satellite lanes written by THIS request, in template
	// order. Every one of them is disabled.
	Created []string `json:"created"`
	// Existing names the satellite lanes that already existed. They are
	// reported, never touched.
	Existing []string `json:"existing"`
	// Notes carries non-fatal deviations from the full family shape (a family
	// namespace this scheduler does not define, so the lane was minted with a
	// null namespace). The lane exists; the note is what an operator has to
	// finish before enabling it.
	Notes []string `json:"notes"`
	// Errors carries per-lane failures (a failed lane does not roll the
	// others back — the caller sees exactly which lane is missing and why).
	Errors []string `json:"errors"`
}

// satelliteLaneFor builds the row for one default-family satellite of primary.
//
// The row is the live family shape, not a new invention: the canonical workdir
// comes from canonicalLaneWorkdirs (the same derivation the onboarding gate
// validates against), the cadence and both bounds from satelliteFamilyPins /
// database.DefaultAdaptiveCooldownCeiling, and repo_url / deliver are inherited
// from the foreman so the family reports where the foreman reports. Weight and
// priority are the satellite defaults the host reconciler uses (4/4).
//
// The namespace is assigned by assignFamilyNamespace, which needs the DB: a
// scheduler that does not define the family's namespace would otherwise fail
// the FK at insert time.
//
// enabled is false: create never arms a row (SCHED-GAP-1729), and the enable
// transition is where the SCHED-GAP-138 auto-arm runs.
func satelliteLaneFor(primary *database.Project, suffix string) (database.Project, error) {
	family := strings.TrimPrefix(suffix, "-")
	pin, ok := satelliteFamilyPins[family]
	if !ok {
		return database.Project{}, fmt.Errorf("no family cadence pin for %q (add it to satelliteFamilyPins)", suffix)
	}
	if _, ok := satelliteFamilyNamespaces[family]; !ok {
		return database.Project{}, fmt.Errorf("no family namespace for %q (add it to satelliteFamilyNamespaces)", suffix)
	}
	dirs := canonicalLaneWorkdirs(primary.Name + suffix)
	if len(dirs) == 0 {
		return database.Project{}, fmt.Errorf("no canonical workdir for %q (add the family to satelliteWorkdirRoots)", suffix)
	}
	ceiling := database.DefaultAdaptiveCooldownCeiling(pin)
	return database.Project{
		Name:             primary.Name + suffix,
		RepoURL:          primary.RepoURL,
		Workdir:          dirs[0],
		Weight:           4,
		Priority:         4,
		CooldownS:        pin,
		CooldownPinS:     &pin,
		CooldownFloorS:   pin,
		CooldownCeilingS: ceiling,
		DecayRate:        1.0,
		Parent:           primary.Name,
		AdmissionMode:    database.AdmissionModeCooldown,
		Deliver:          primary.Deliver,
		DeliverMode:      primary.DeliverMode,
		Enabled:          false,
	}, nil
}

// assignFamilyNamespace points lane at its family's namespace and reports
// whether the namespace exists on THIS scheduler. A missing namespace is not a
// failure — the lane is still a valid un-namespaced row and the caller records
// a note — but it must never be written as a dangling FK reference, which is
// exactly what the insert would reject.
func assignFamilyNamespace(ctx context.Context, db *sql.DB, lane *database.Project, suffix string) bool {
	nsID := satelliteFamilyNamespaces[strings.TrimPrefix(suffix, "-")]
	if _, err := database.GetNamespace(ctx, db, nsID); err != nil {
		lane.NamespaceID = nil
		return false
	}
	ns := nsID
	lane.NamespaceID = &ns
	return true
}

// familyLaneExists classifies a CreateProject failure against the lane it was
// writing: true means "this lane is already there" (the idempotent case), false
// means it is a real defect to report.
//
// Two database errors mean the row exists. A DISABLED pre-existing row is
// caught by the INSERT itself ("UNIQUE constraint failed: projects.name"); an
// ENABLED one is caught one layer earlier by CreateProject's duplicate guards,
// which name the occupying project — and only a guard naming THIS lane proves
// this lane exists (a guard naming some other lane is a genuine workdir/name
// conflict that must surface).
func familyLaneExists(err error, name string) bool {
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint") {
		return true
	}
	const marker = "already registered by enabled project "
	idx := strings.LastIndex(msg, marker)
	if idx < 0 {
		return false
	}
	owner := msg[idx+len(marker):]
	owner = strings.TrimPrefix(owner, `"`)
	if end := strings.Index(owner, `"`); end >= 0 {
		owner = owner[:end]
	}
	return owner == name
}

// mintDefaultFamily writes the default family satellites for primary.
//
// Every row is validated by validateLaneOnCreate BEFORE any write, so a minted
// lane can never be a shape a hand-written create would have been refused for
// (the gate would otherwise only catch it on the next fleet-wide check). A
// lane that already exists is reported and left untouched — the mint is
// additive, and re-running it over a provisioned fleet must change nothing.
// Any other failure is collected in result.Errors and the remaining lanes are
// still attempted, because a half-provisioned family that NAMES its gap is more
// useful to an operator than a silent abort.
func mintDefaultFamily(ctx context.Context, db *sql.DB, primary *database.Project) familyMintResult {
	res := familyMintResult{
		DefaultRoles:  scheduler.DefaultFamilyRoles(),
		OnDemandRoles: scheduler.OnDemandFamilyRoles(),
		Created:       []string{},
		Existing:      []string{},
		Notes:         []string{},
		Errors:        []string{},
	}
	for _, suffix := range res.DefaultRoles {
		lane, err := satelliteLaneFor(primary, suffix)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		if !assignFamilyNamespace(ctx, db, &lane, suffix) {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: namespace %q is not registered on this scheduler — minted with namespace_id null; assign one before enabling",
				lane.Name, satelliteFamilyNamespaces[strings.TrimPrefix(suffix, "-")]))
		}
		// The SCHED-GAP-138 onboarding shape gate, applied to the row this
		// file is about to write. A template that could mint a lane the gate
		// refuses would be a template that breaks its own contract.
		if err := validateLaneOnCreate(&lane); err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		if err := database.CreateProject(ctx, db, &lane); err != nil {
			if familyLaneExists(err, lane.Name) {
				res.Existing = append(res.Existing, lane.Name)
				continue
			}
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", lane.Name, err))
			continue
		}
		res.Created = append(res.Created, lane.Name)
	}
	return res
}

// writeFamilyCreated answers a family onboarding create: the foreman row plus
// the mint report.
//
// The 201 body is a wrapper rather than the bare Project because the caller
// asked for four rows, and a bare row would say nothing about the three
// satellites (whether they were created, already existed, or failed). A
// request WITHOUT the family flag keeps the historical bare-Project body
// exactly as it was.
func (s *Server) writeFamilyCreated(w http.ResponseWriter, primary *database.Project, res familyMintResult) {
	status := http.StatusCreated
	body := map[string]any{
		"project": primary,
		"family":  res,
	}
	if len(res.Errors) > 0 {
		// The foreman row landed — this is a partial provisioning, not a
		// failed create. 500 names it so an operator (or a script that gates
		// on the status) cannot mistake a one-lane family for a four-lane one.
		status = http.StatusInternalServerError
		body["error"] = fmt.Sprintf("family onboarding incomplete: %s", strings.Join(res.Errors, "; "))
	}
	writeJSON(w, status, body)
}
