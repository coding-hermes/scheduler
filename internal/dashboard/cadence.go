package dashboard

// SCHED-GAP-1746: the Cadence & Residue page. Cadence (configured vs
// achieved starts/day, SOL-CADENCE / SCHED-GAP-1668) and the cooldown-pin
// residue detector (SCHED-GAP-1670) existed only on REST/MCP surfaces
// (/api/v1/cadence, /api/v1/cooldown/residue); this page renders both from
// the SAME database resolution the API endpoints use — no second formula,
// no dashboard-shaped re-derivation:
//
//   - cadence rows: database.ListProjects + LoadCadenceRates +
//     EffectiveCadenceTarget (the exact trio /api/v1/cadence runs);
//   - residue rows: database.ListCooldownResidue (the exact query the
//     detector endpoint serves).
//
// The dashboard architecture cannot call the REST endpoints in-process
// (the generator holds no API handler for reads), so the data path here is
// the same database package functions the handlers call — the brief's
// sanctioned alternative to an internal HTTP round-trip. Every link to a
// REST surface on the page points at a route main.go really serves.

import (
	"context"
	"io"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// CadenceResidueRoute is the residue detector's REST route, rendered as an
// explicit operator link on the page. The endpoint is served by
// internal/api (SCHED-GAP-1670) and pinned in docs/reference/endpoints.md;
// the template's copy of the literal is guarded by
// TestSCHEDGAP1746_CadenceResiduePageRendersBothDatasources.
const CadenceResidueRoute = "/api/v1/cooldown/residue"

// cadenceWindow is the SAME trailing window the API endpoint serves
// (internal/api/server_cadence.go). It lives there as a private const, so
// the value is repeated here with a test (TestSCHEDGAP1746_WindowMatchesAPI)
// pinning the two together — one number, two surfaces, one guard.
const cadenceWindow = 7 * 24 * time.Hour

// cadenceRow is one lane's cadence render row. Target/deficit stay as
// pointers so the "no cadence opinion" (target_source none/disabled) case
// renders an explicit —, never a fabricated 0/day target.
type cadenceRow struct {
	Name         string
	Target       *float64
	Source       string
	Achieved     float64
	Deficit      float64
	WindowDays   int
	Enabled      bool
	InResidue    bool // live cooldown below the lane's own pin
	PinnedByUser bool // a durable cooldown pin exists (the derived-target source)
}

// residueRow is one lane of the cooldown-residue detector: live cooldown
// strictly below the lane's own stored operator pin.
type residueRow struct {
	Name     string
	Cooldown int
	Pin      int
	PinBy    string
}

// CadenceData is the page's render payload.
type CadenceData struct {
	Title       string
	GeneratedAt string
	// Cadence is every stored lane (enabled AND disabled, ordered by name),
	// exactly the row set /api/v1/cadence serves.
	Cadence []cadenceRow
	// Residue is the detector's lanes: ENABLED, live < own pin, by name.
	// Empty (not nil-checked in templates — len guards) when the fleet is
	// clean; the template renders the explicit clean state for that arm.
	Residue []residueRow
	// ResidueRoute is the detector endpoint link (CadenceResidueRoute).
	ResidueRoute string
	// CadenceRoute is the cadence endpoint link (/api/v1/cadence).
	CadenceRoute string
	// WindowDays repeats the window on the payload so the header text and
	// the rows can never disagree about it.
	WindowDays int
	// DataError is set when a store read failed: the page renders an
	// explicit unavailable notice instead of empty tables presented as a
	// complete view (the Remote section's §8 rendering law).
	DataError string
}

// cadenceData assembles the page's facts; shared by the full page and the
// htmx residue fragment (both need the current residue set).
func (g *Generator) cadenceData() CadenceData {
	ctx := context.Background()
	data := CadenceData{
		Title:        "Cadence & Residue",
		GeneratedAt:  g.clock().Now().UTC().Format(time.RFC3339),
		ResidueRoute: CadenceResidueRoute,
		CadenceRoute: "/api/v1/cadence",
		WindowDays:   int(cadenceWindow.Hours() / 24),
	}

	projects, err := database.ListProjects(ctx, g.db, false)
	if err != nil {
		data.DataError = err.Error()
		return data
	}
	rates, err := database.LoadCadenceRates(ctx, g.db, g.clock().Now(), cadenceWindow)
	if err != nil {
		data.DataError = err.Error()
		return data
	}
	residue, err := database.ListCooldownResidue(ctx, g.db)
	if err != nil {
		data.DataError = err.Error()
		return data
	}

	inResidue := make(map[string]bool, len(residue))
	rows := make([]cadenceRow, 0, len(projects))
	for i := range projects {
		p := projects[i]
		target, source, configured := database.EffectiveCadenceTarget(p)
		var targetPtr *float64
		deficit := 0.0
		if configured {
			targetPtr = &target
			deficit = target - rates[p.Name]
			if deficit < 0 {
				deficit = 0
			}
		}
		rows = append(rows, cadenceRow{
			Name:         p.Name,
			Target:       targetPtr,
			Source:       source,
			Achieved:     rates[p.Name],
			Deficit:      deficit,
			WindowDays:   data.WindowDays,
			Enabled:      p.Enabled,
			InResidue:    false,
			PinnedByUser: p.CooldownPinS != nil && *p.CooldownPinS > 0,
		})
	}

	resRows := make([]residueRow, 0, len(residue))
	for _, r := range residue {
		inResidue[r.Name] = true
		resRows = append(resRows, residueRow{
			Name:     r.Name,
			Cooldown: r.CooldownS,
			Pin:      r.PinS,
			PinBy:    r.PinBy,
		})
	}
	// Mark the cadence rows AFTER the residue set is known (one pass, no
	// per-row re-query — GAP-043 keeps per-request scans bounded).
	for i := range rows {
		rows[i].InResidue = inResidue[rows[i].Name]
	}

	data.Cadence = rows
	data.Residue = resRows
	return data
}

// GenerateCadence renders the full /cadence page.
func (g *Generator) GenerateCadence(w io.Writer) error {
	return g.cadenceTmpl.Execute(w, g.cadenceData())
}

// GenerateCadenceResidueRows renders only the residue rows fragment for htmx
// polling. The page's tbody polls /cadence/partial with hx-swap=innerHTML —
// the response must be the fragment, never a full page swapped into its own
// poller (the /health and /remote fragment contract).
func (g *Generator) GenerateCadenceResidueRows(w io.Writer) error {
	return g.cadenceTmpl.ExecuteTemplate(w, "cadence_residue_rows", g.cadenceData())
}
