package config

// Lane pin validation against the router registry (CF-245).
//
// A lane may pin a (model, provider) pair in fleet.toml. That pin OUTRANKS the
// router's own ranking, so a pin naming a pair the registry marks disabled —
// or a pair that is not in the registry at all — is not a preference, it is a
// lane that cannot be served. Measured cost of getting this wrong: the
// code-factory lane carried model="deepseek-v4-flash" against
// provider="deepseek-foreman", a pair that has never existed in
// task-router/data/tables/models.jsonl (deepseek-foreman serves deepseek-flash,
// deepseek-v4-pro, deepseek-v4-flash-vision-exp), and nothing reported it.
// Across the fleet 215 lanes carry a pin and 2 named unservable pairs.
//
// The check is deliberately BEST-EFFORT and never fails a config load:
//
//   - the registry is a file in ANOTHER repository, absent on hosts that only
//     run the scheduler, so an unreadable registry yields no problems rather
//     than a fleet that will not start;
//   - a registry that a sibling version owns may legitimately lag a pin being
//     added ahead of it, so the finding is a WARNING naming the lane and the
//     pair, not a load error.
//
// What it buys is attribution: the next unservable pin is named at startup
// instead of being discovered as 177 dead ticks across 157 lanes.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PinRegistry maps "model/provider" to whether the router marks it disabled.
// A pair absent from the map is unknown to the registry, which is its own
// finding — a pin can be unservable by being unknown, not only by being off.
type PinRegistry map[string]bool

// pinKey renders the pair the way the registry is keyed.
func pinKey(model, provider string) string {
	return strings.TrimSpace(model) + "/" + strings.TrimSpace(provider)
}

// DefaultPinRegistryPath is where the router keeps its model table on a host
// that runs both. It is overridable via SCHEDULER_PIN_REGISTRY so a host with
// a different layout (or a fixture in a test) can point elsewhere.
const DefaultPinRegistryPath = "/home/kara/task-router/data/tables/models.jsonl"

// PinRegistryPath resolves the registry location: the environment override
// first, then the documented default. It does not check existence — callers
// that need to distinguish "absent" from "empty" should stat the result.
func PinRegistryPath() string {
	if p := strings.TrimSpace(os.Getenv("SCHEDULER_PIN_REGISTRY")); p != "" {
		return p
	}
	return DefaultPinRegistryPath
}

// LoadPinRegistry reads a JSONL model table ({model, provider, disabled}) into
// a PinRegistry. Rows missing a model or provider are skipped rather than
// fatal: the table carries other shapes and a validator must not refuse to
// load because a sibling wrote a row this check does not understand.
func LoadPinRegistry(path string) (PinRegistry, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()

	reg := make(PinRegistry, 256)
	sc := bufio.NewScanner(fh)
	// The table's rows are short, but a future row carrying a long notes field
	// must not silently truncate the scan and hide the rest of the registry.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Disabled bool   `json:"disabled"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if row.Model == "" || row.Provider == "" {
			continue
		}
		reg[pinKey(row.Model, row.Provider)] = row.Disabled
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return reg, nil
}

// PinProblem is one lane whose pin the router cannot serve.
type PinProblem struct {
	Lane     string
	Model    string
	Provider string
	Reason   string // "not-in-registry" or "disabled"
}

// String renders the finding the way an operator reads it in a log line.
func (p PinProblem) String() string {
	return fmt.Sprintf("lane %q pins %s/%s which the router registry reports %s",
		p.Lane, p.Model, p.Provider, p.Reason)
}

// ValidatePins reports every project whose pinned pair the registry cannot
// serve. Pins naming an empty model or provider are skipped: an empty pin is
// "no pin" (the loader fills the default), not an unservable pair.
//
// Results are ordered by lane so two runs over the same config diff cleanly.
func ValidatePins(projects []ProjectDef, reg PinRegistry) []PinProblem {
	if len(reg) == 0 {
		return nil
	}
	var out []PinProblem
	for _, p := range projects {
		if strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Provider) == "" {
			continue
		}
		disabled, known := reg[pinKey(p.Model, p.Provider)]
		switch {
		case !known:
			out = append(out, PinProblem{Lane: p.Name, Model: p.Model, Provider: p.Provider, Reason: "not-in-registry"})
		case disabled:
			out = append(out, PinProblem{Lane: p.Name, Model: p.Model, Provider: p.Provider, Reason: "disabled"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lane < out[j].Lane })
	return out
}

// LanePinProblems validates the projects of cfg against the registry at path.
// It NEVER returns an error: a registry that cannot be read yields no findings,
// because a validator in a second repository must not be able to stop the
// scheduler from starting. Callers that want to say "the registry was absent"
// should stat PinRegistryPath() themselves and say so.
func LanePinProblems(cfg *RootConfig, path string) []PinProblem {
	if cfg == nil || len(cfg.Projects) == 0 {
		return nil
	}
	reg, err := LoadPinRegistry(path)
	if err != nil || len(reg) == 0 {
		return nil
	}
	return ValidatePins(cfg.Projects, reg)
}

// pinRegistryAvailable reports whether a registry file is present and readable,
// so a caller can distinguish "no problems" from "could not check".
func pinRegistryAvailable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// PinRegistryPathFromConfigDir is a convenience for hosts that keep the
// registry beside the fleet config rather than at the router's repo root.
// It returns "" when neither location exists.
func PinRegistryPathFromConfigDir(configPath string) string {
	if p := PinRegistryPath(); pinRegistryAvailable(p) {
		return p
	}
	if configPath == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(configPath), "models.jsonl")
	if pinRegistryAvailable(candidate) {
		return candidate
	}
	return ""
}

// WarnUnservablePins writes one line per lane whose pin the registry cannot
// serve. Silent when the registry is absent (nothing to check against) and
// silent when every pin resolves — an operator who sees nothing can trust that
// the check ran and found nothing, because the absence of the registry is
// distinguishable via pinRegistryAvailable in the caller's own diagnostics.
func WarnUnservablePins(cfg *RootConfig, configPath string) {
	path := PinRegistryPathFromConfigDir(configPath)
	if path == "" {
		return
	}
	problems := LanePinProblems(cfg, path)
	if len(problems) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "config: %d lane pin(s) name a (model, provider) pair %s cannot serve (CF-245):\n", len(problems), path)
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "  WARN %s\n", p.String())
	}
}
