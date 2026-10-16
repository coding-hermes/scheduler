package config

import "fmt"

// ParseForemanRoutingMode validates the opt-in orchestration policy. Values
// are exact: accepting a typo as router would silently unpin the foreman.
func ParseForemanRoutingMode(value string) (string, error) {
	switch value {
	case "", "router":
		return "router", nil
	case "fixed":
		return "fixed", nil
	default:
		return "", fmt.Errorf("SCHEDULER_FOREMAN_ROUTING_MODE / scheduler.foreman_routing_mode: expected router or fixed, got %q", value)
	}
}
