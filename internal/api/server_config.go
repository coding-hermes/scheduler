package api

import (
	"net/http"
	"time"

	"github.com/coding-herms/scheduler/internal/scheduler"
)

// GatewayConfigSnapshot is the gateway section of the resolved config
// exposed by GET /api/v1/config. The key is masked at set time — the
// plaintext gateway key never reaches the wire.
type GatewayConfigSnapshot struct {
	URL            string `json:"url"`
	Key            string `json:"key"` // masked (first 4 chars + "****"); empty when unset
	ForemanHome    string `json:"foreman_home"`
	NoExecFallback bool   `json:"no_exec_fallback"`
}

// DuckBrainConfigSnapshot is the duckbrain section of the resolved config
// exposed by GET /api/v1/config.
type DuckBrainConfigSnapshot struct {
	Namespace string `json:"namespace"`
	URL       string `json:"url"`
}

// ResolvedConfig is a startup-time snapshot of the daemon's ACTIVE
// three-layer configuration (TOML file < env vars < CLI flags), captured
// in main.go after TOML/env resolution and exposed read-only via
// GET /api/v1/config for operator introspection (SCHED-GAP-034).
// Durations are rendered as Go duration strings (e.g. "30s", "2h").
type ResolvedConfig struct {
	DBPath                 string                  `json:"db_path"`
	Listen                 string                  `json:"listen"`
	MinInterval            string                  `json:"min_interval"`
	MaxInterval            string                  `json:"max_interval"`
	NumLevels              int                     `json:"num_levels"`
	WeightBudget           int                     `json:"weight_budget"`
	MaxConcurrent          int                     `json:"max_concurrent"`
	TickTimeout            string                  `json:"tick_timeout"`
	NamespaceMode          bool                    `json:"namespace_mode"`
	AutoDisableFailureRate float64                 `json:"auto_disable_failure_rate"`
	AutoDisableWindow      int                     `json:"auto_disable_window"`
	AutoDisableMinTicks    int                     `json:"auto_disable_min_ticks"`
	FailureWindow          int                     `json:"failure_window"`
	Gateway                GatewayConfigSnapshot   `json:"gateway"`
	DuckBrain              DuckBrainConfigSnapshot `json:"duckbrain"`
}

// SetResolvedConfig stores the resolved-config snapshot served by
// GET /api/v1/config (SCHED-GAP-034). The gateway key is masked before
// storage so the plaintext key can never leak through the endpoint.
// It also (re)builds the urgency calculator for GET /api/v1/queue (GAP-054)
// from the resolved interval range; an absent or unparseable range leaves
// the calculator nil (listQueue falls back to priority-only scores).
func (s *Server) SetResolvedConfig(cfg ResolvedConfig) {
	cfg.Gateway.Key = maskGatewayKey(cfg.Gateway.Key)
	s.resolvedConfig = cfg
	s.urgencyCalc = newUrgencyCalculatorFromConfig(cfg)
}

// newUrgencyCalculatorFromConfig builds the scheduler's urgency calculator
// from a resolved interval range, or nil when the range is missing or
// unparseable (MinInterval/MaxInterval must parse as Go durations and
// NumLevels must be > 0). It mirrors the daemon's NewUrgencyCalculator call
// in internal/scheduler/loop.go so the API's urgency scores match the
// engine's ordering exactly.
func newUrgencyCalculatorFromConfig(cfg ResolvedConfig) *scheduler.UrgencyCalculator {
	if cfg.NumLevels <= 0 {
		return nil
	}
	minI, err := time.ParseDuration(cfg.MinInterval)
	if err != nil {
		return nil
	}
	maxI, err := time.ParseDuration(cfg.MaxInterval)
	if err != nil {
		return nil
	}
	return scheduler.NewUrgencyCalculator(minI, maxI, cfg.NumLevels)
}

// urgencyCalculator returns the server's urgency calculator, lazily
// rebuilding it from the resolved-config snapshot on first use when
// SetResolvedConfig did not produce one (e.g. an unparseable range at set
// time that is later corrected). Returns nil when no usable config exists.
func (s *Server) urgencyCalculator() *scheduler.UrgencyCalculator {
	if s.urgencyCalc != nil {
		return s.urgencyCalc
	}
	s.urgencyCalc = newUrgencyCalculatorFromConfig(s.resolvedConfig)
	return s.urgencyCalc
}

// maskGatewayKey masks a gateway API key for introspection: first 4 chars
// plus "****" (empty stays empty, short keys become "****").
func maskGatewayKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return key[:4] + "****"
}

// config returns the resolved three-layer configuration snapshot
// (TOML < env vars < CLI flags) captured at startup (SCHED-GAP-034).
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	writeJSON(w, 200, s.resolvedConfig)
}
