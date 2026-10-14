package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// Lane onboarding shape gate (SCHED-GAP-138).
//
// The live instances of the three defects below were repaired in place, but
// nothing checked the SHAPE of a lane arriving through the API, so the next
// onboarding session could re-emit them (measured 2026-09-17 on the freshly
// registered `trouble` family):
//
//  1. a retired driver command — already refused by retired_drivers.go
//     (SCHED-GAP-150); this file adds nothing there, it is named because the
//     three defects arrived together;
//  2. an off-convention workdir — `trouble-sync` was registered pointing at
//     ~/.hermes/stand-in/pm/trouble-sync while every other sync lane lives in
//     ~/.hermes/sync-workdirs/;
//  3. an unarmed row — enabled with adaptive_cooldown=0, floor=0, ceiling=0,
//     i.e. no pacing policy at all.
//
// Placement is deliberate and mirrors the SCHED-GAP-150 gate: the refusal
// lives on the API write path (createProject / updateProject), NOT inside
// database.CreateProject/UpdateProject. Internal writers — the fleet.toml
// loader, the pause/resume satellite cascade, the scheduler's own arm pass —
// legitimately install rows the operator never typed, and a boot-time or
// cascade-time refusal would strand the fleet instead of teaching the caller.
// The API is where an onboarding session (or the satellite reconciler) writes,
// so the API is where the shape is enforced.
//
// Every rule here is the write-time half of an invariant that
// ops/check-fleet-invariants.py enforces over the live fleet (checks 5/5b/5e):
// the script catches drift after the fact, this gate refuses to create it.
// Parity is pinned by the *_Parity tests in schedgap138_onboarding_test.go.

// laneSatelliteFamily returns the satellite family ("qa", "pm", "sync",
// "dogfood") of a lane name, or "" when the name is a primary/root lane.
//
// The suffix vocabulary is satelliteLaneSuffixes (server_projects.go) — the
// same list the pause/resume cascade and the dashboard's lane nesting use, so
// adding a family there extends this gate too instead of silently bypassing it.
func laneSatelliteFamily(name string) string {
	for _, suffix := range satelliteLaneSuffixes {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimPrefix(suffix, "-")
		}
	}
	return ""
}

// satelliteWorkdirRoots maps a satellite family onto the directories, relative
// to the Hermes home, where its lanes live. Each entry is a MEASURED live
// convention (census 2026-09-28 over every satellite row in the fleet DB), not
// a preference: the -qa family sits in stand-in/pm (53 lanes) and stand-in/qa
// (4), the -pm family in stand-in/pm (23) and stand-in/pm-lane (34), -sync in
// sync-workdirs (65) and -dogfood in stand-in/dogfood (57) — no satellite row
// lived anywhere else (the -releng family was added later, SCHED-GAP-1733:
// each releng lane gets its own leaf stand-in/releng-lane/<primary>, matching
// ~/.hermes/scripts/complete_satellite_families.py and asce_align.py). The
// host-side provisioning script
// (~/.hermes/scripts/satellite-coverage-reconcile.py, outside this repo) writes
// exactly stand-in/pm for -qa, stand-in/pm-lane for -pm, sync-workdirs for
// -sync and stand-in/dogfood for -dogfood (and stand-in/releng-lane for
// -releng); all five are accepted here, so the
// canonical provisioning path can never be refused by this gate. Adding a
// family to satelliteLaneSuffixes without adding its roots here is a bug: the
// *_Parity test asserts the key sets agree.
var satelliteWorkdirRoots = map[string][]string{
	"qa":      {"stand-in/pm", "stand-in/qa"},
	"pm":      {"stand-in/pm", "stand-in/pm-lane"},
	"sync":    {"sync-workdirs"},
	"dogfood": {"stand-in/dogfood"},
	"releng":  {"stand-in/releng-lane"},
}

// satelliteFamilyPins is the per-family cadence pin an enabled satellite
// carries (seconds), for the five families this onboarding gate POLICES (its
// workdir/arming vocabulary in satelliteLaneSuffixes). Every value here MUST
// equal the matching entry of SATELLITE_FAMILY_PINS in
// ops/check-fleet-invariants.py — the canonical cadence matrix (Bane's
// 2026-09-29 alignment, SCHED-GAP-1675, mirrored as FAMILY_CANONICAL in
// ~/.hermes/scripts/fleet-cooldown-policy.py) — which carries four further
// families (perf/review/readme/docs) this gate does not onboard.
// TestSCHEDGAP138_FamilyConstantsParity reads both sources and fails the build
// the moment either side is edited alone. The number is also the one the
// refusal message hands an operator (unarmedLaneError) and the floor
// laneAutoArm writes on an enable transition, so it must be the cadence the
// fleet actually pins: qa moved 21600 -> 43200 with the 2026-10-02 ruling
// (12h — qa burned 32% of foremen slot-hours for 6% of commits,
// docs/scheduler-rules.md R4.2), sync stays at its 6h family cadence, and
// the derived ceiling follows at 8 x floor.
// The host-side reconciler carries the same four numbers as FAM_CD.
var satelliteFamilyPins = map[string]int{
	"qa":      43200,
	"pm":      86400,
	"sync":    21600,
	"dogfood": 259200,
	"releng":  86400,
}

// Out-of-scope families (SCHED-GAP-1733 criterion 2): perf/review/readme/docs
// remain offboarded by this gate — the fleet-topology warden's --apply does
// not CREATE those lanes (no live provisioning convention dir exists for
// them), so onboarding them here would be dead vocabulary. FAMILY_CANONICAL
// in ~/.hermes/scripts/fleet-cooldown-policy.py does carry cadence pins for
// them (weekly cadence for all four), and ops/check-fleet-invariants.py's
// SATELLITE_FAMILY_PINS mirrors that, but a pin alone is not a workdir
// convention: until a measured live directory exists (like
// stand-in/releng-lane for -releng), they stay out of satelliteWorkdirRoots,
// satelliteFamilyPins, and the *_Parity key-set assertions.

// hermesHomeDir resolves the Hermes home (~/.hermes) the way the ops scripts
// do: HERMES_HOME when set (it IS the .hermes directory — see
// policyScriptPath), else the running user's home plus .hermes. Never a
// hardcoded /home/kara, so the gate works for any host and any test rig.
func hermesHomeDir() string {
	if hh := os.Getenv("HERMES_HOME"); hh != "" {
		return hh
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".hermes")
	}
	return "/home/kara/.hermes"
}

// canonicalLaneWorkdirs returns every accepted absolute workdir for a lane, in
// preference order, or nil for a primary/root lane (unconstrained).
//
// The directory LEAF differs by family, and that difference is the live
// convention: a sync lane gets a workdir named after ITSELF
// (sync-workdirs/trouble-sync — every one of the 65 live sync lanes matches),
// while the other families share the primary's stand-in directory
// (stand-in/pm/<primary>, stand-in/qa/<primary>, stand-in/pm-lane/<primary>,
// stand-in/dogfood/<primary>).
func canonicalLaneWorkdirs(name string) []string {
	family := laneSatelliteFamily(name)
	if family == "" {
		return nil
	}
	leaf := name
	if family != "sync" {
		leaf = strings.TrimSuffix(name, "-"+family)
	}
	roots := satelliteWorkdirRoots[family]
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		out = append(out, filepath.Join(hermesHomeDir(), filepath.FromSlash(root), leaf))
	}
	return out
}

// validateLaneWorkdir refuses an off-convention workdir for a satellite lane.
//
// It runs on every write that sets a workdir (create and update) and is
// deliberately independent of `enabled`: the workdir is the lane's identity on
// this host, and a disabled lane registered in the wrong directory is the same
// defect one resume away from being live. Empty is refused for a satellite —
// create requires a workdir, and a PUT that blanks it would leave the lane
// unresolvable.
func validateLaneWorkdir(name, workdir string) error {
	family := laneSatelliteFamily(name)
	if family == "" {
		return nil
	}
	accepted := canonicalLaneWorkdirs(name)
	if workdir != "" {
		got := filepath.Clean(workdir)
		for _, want := range accepted {
			if got == want {
				return nil
			}
		}
	}
	return fmt.Errorf("lane %q workdir %q is off-convention: -%s lanes live in %s",
		name, workdir, family, quotedOrJoin(accepted))
}

// quotedOrJoin renders an accepted-path list for an error message:
// `"/a/x" or "/b/x"` when there are two, `"/a/x"` when there is one.
func quotedOrJoin(paths []string) string {
	quoted := make([]string, 0, len(paths))
	for _, p := range paths {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	return strings.Join(quoted, " or ")
}

// laneIsUnarmed reports whether a lane row carries no pacing policy at all:
// adaptive cooldown off and both bound fields zero. This is the exact shape
// measured on the `trouble` row (enabled=1, adaptive_cooldown=0, floor=0,
// ceiling=0) that the arming tripwire caught.
func laneIsUnarmed(adaptive bool, floor, ceiling int) bool {
	return !adaptive && floor <= 0 && ceiling <= 0
}

// unarmedLaneError is the actionable refusal for an enabled satellite with no
// pacing policy. It names the family pin the fleet's own reconciler writes
// (FAM_CD) so the fix is a single number from the response alone.
//
// Note the remedy it names: satellites are paced by their FAMILY FLOOR — the
// live fleet forbids arming a satellite with adaptive cooldown
// (ops/check-fleet-invariants.py check 5b, "disarm, don't debate"). An
// adaptive_cooldown=true row is therefore accepted by laneIsUnarmed (it IS a
// pacing policy) but is independently flagged by that check; the floor pin is
// the shape this gate recommends.
func unarmedLaneError(name, family string) error {
	return fmt.Errorf("enabled satellite lane %q is unarmed: set cooldown_floor_s=%d (the -%s family pin) so the lane is paced instead of drifting",
		name, satelliteFamilyPins[family], family)
}

// validateLaneOnCreate refuses a create body that would register a satellite
// lane in an off-convention workdir or with no pacing policy. It runs before
// any DB write, so a refused create leaves no row behind.
func validateLaneOnCreate(p *database.Project) error {
	if err := validateLaneWorkdir(p.Name, p.Workdir); err != nil {
		return err
	}
	return validateLaneArming(p.Name, p.Enabled, p.AdaptiveCooldown, p.CooldownFloorS, p.CooldownCeilingS)
}

// validateLaneArming refuses an ENABLED satellite lane that would end up with
// no pacing policy. A disabled row may be registered unarmed: create never
// auto-enables, and the reconciler provisions rows disabled and enables them
// afterwards, so demanding a pin on a parked row would refuse the shape the
// fleet's own provisioning path writes.
func validateLaneArming(name string, enabled, adaptive bool, floor, ceiling int) error {
	family := laneSatelliteFamily(name)
	if family == "" || !enabled || !laneIsUnarmed(adaptive, floor, ceiling) {
		return nil
	}
	return unarmedLaneError(name, family)
}

// laneUpdateArming resolves the arming state a PUT would leave behind: the
// stored row with every arming field the update names overridden. Returned as
// (adaptive, floor, ceiling) so callers can evaluate laneIsUnarmed on the
// EFFECTIVE row rather than on the patch — a request that enables a lane AND
// pins its floor in one body is armed, not unarmed.
func laneUpdateArming(cur *database.Project, updates database.ProjectUpdates) (adaptive bool, floor, ceiling int) {
	adaptive, floor, ceiling = cur.AdaptiveCooldown, cur.CooldownFloorS, cur.CooldownCeilingS
	if updates.AdaptiveCooldown != nil {
		adaptive = *updates.AdaptiveCooldown
	}
	if updates.CooldownFloorS != nil {
		floor = *updates.CooldownFloorS
	}
	if updates.CooldownCeilingS != nil {
		ceiling = *updates.CooldownCeilingS
	}
	return adaptive, floor, ceiling
}

// validateLaneUpdate refuses a PUT that would leave an enabled satellite with
// no pacing policy — an update that explicitly writes an arming field
// (adaptive_cooldown / cooldown_floor_s / cooldown_ceiling_s) and zeroes the
// lane's last bound, e.g. clearing a floor on a non-adaptive lane, or
// switching adaptive off on a lane whose only policy was adaptive.
//
// It is deliberately scoped to writes that NAME an arming field. A row that is
// already enabled-and-unarmed (29 legacy lanes in the live fleet) stays
// updatable: an unrelated field write — a weight bump, a namespace move — is
// not the write that installed that shape, and refusing it here would strand
// rows this gate did not create (the fleet-wide invariant gate reports them).
// The rule reads the EFFECTIVE post-update row, so a body that enables a lane
// and pins its floor in one request passes.
func validateLaneUpdate(cur *database.Project, updates database.ProjectUpdates) error {
	family := laneSatelliteFamily(cur.Name)
	if family == "" || !laneUpdateTouchesArming(updates) {
		return nil
	}
	enabled := cur.Enabled
	if updates.Enabled != nil {
		enabled = *updates.Enabled
	}
	if !enabled {
		return nil
	}
	adaptive, floor, ceiling := laneUpdateArming(cur, updates)
	if !laneIsUnarmed(adaptive, floor, ceiling) {
		return nil
	}
	return unarmedLaneError(cur.Name, family)
}

// laneUpdateTouchesArming reports whether the update writes any of the three
// arming fields — the write that can install (or remove) a lane's pacing
// policy. See validateLaneUpdate for why the scope matters.
func laneUpdateTouchesArming(updates database.ProjectUpdates) bool {
	return updates.AdaptiveCooldown != nil || updates.CooldownFloorS != nil || updates.CooldownCeilingS != nil
}

// laneAutoArm is the ENABLE-TRANSITION repair: an update that switches a
// satellite lane on (`enabled: true` on a currently disabled row) while it
// carries no pacing policy gets the family pin applied in the same write —
// floor = the family pin, ceiling = the ADV-R10 derived cap (8 × floor) when
// the row has none.
//
// Why repair instead of refuse on this one path: the enable transition is
// written by machinery, not by an onboarding author. satellite-coverage-
// reconcile.py enables every satellite of a live primary with a bare
// `PUT {"enabled": true}` and does NOT read the response status — a 400 there
// would be an invisible no-op that silently strips QA/dogfood/sync coverage
// from a live foreman. Repairing keeps the operator's intent (the lane runs)
// and removes the unarmed shape anyway, which is the whole point of the gate.
//
// Scope is exactly that transition. An unrelated update to a row that is
// ALREADY enabled-and-unarmed does not silently rewrite it (those 29 legacy
// rows are the fleet gate's to report, not this handler's to mutate behind an
// unrelated weight bump), and an explicit disarm is refused by
// validateLaneUpdate before this runs. The returned bool is true when the
// family pin was applied, so the caller can log the repair.
func laneAutoArm(cur *database.Project, updates database.ProjectUpdates, name string) (database.ProjectUpdates, bool) {
	family := laneSatelliteFamily(name)
	if family == "" || updates.Enabled == nil || !*updates.Enabled || cur.Enabled {
		return updates, false
	}
	adaptive, floor, ceiling := laneUpdateArming(cur, updates)
	if !laneIsUnarmed(adaptive, floor, ceiling) {
		return updates, false
	}
	pin := satelliteFamilyPins[family]
	ceilingPin := database.DefaultAdaptiveCooldownCeiling(pin)
	updates.CooldownFloorS = &pin
	updates.CooldownCeilingS = &ceilingPin
	return updates, true
}
