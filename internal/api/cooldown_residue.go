package api

import (
	"log"
	"net/http"

	"github.com/coding-hermes/scheduler/internal/database"
)

// cooldownResidue reports the ENABLED lanes whose live cooldown_s sits
// strictly BELOW their own stored operator pin (SCHED-GAP-1670). Read-only
// by contract: the endpoint never mutates — reconciliation belongs to the
// pin-snap write paths (SetCooldownPin on import, the API PUT arm), not to
// a detection surface. The scheduled residue report reads this same data
// through the loop's daily pass, and the mutating fleet-cooldown-policy.py
// --apply stays OUTSIDE the scheduler on purpose.
func (s *Server) cooldownResidue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	residue, err := database.ListCooldownResidue(r.Context(), s.db)
	if err != nil {
		log.Printf("cooldown residue endpoint: %v", err)
		writeError(w, 500, err.Error())
		return
	}
	if residue == nil {
		// Never null — an empty residue list serializes as [] so ops
		// tooling can iterate without a nil check.
		residue = []database.CooldownResidue{}
	}
	writeJSON(w, 200, map[string]interface{}{
		"residue": residue,
		"count":   len(residue),
	})
}
