package main

import (
	"os"

	"github.com/coding-hermes/scheduler/internal/config"
)

// resolveForemanRoutingMode runs before any boot side effects and the config
// introspection exits. Unlike flag-backed knobs, this policy has no CLI layer.
func resolveForemanRoutingMode(path string) (string, error) {
	value := os.Getenv("SCHEDULER_FOREMAN_ROUTING_MODE")
	if path != "" {
		cfg, err := config.LoadRootConfig(path)
		if err != nil {
			return "", err
		}
		value = cfg.Scheduler.ForemanRoutingMode // loader already applies env > TOML
	}
	return config.ParseForemanRoutingMode(value)
}
