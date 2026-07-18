package main

// This file implements the FEAT-005 `schedulerd schema` subcommand: it
// emits a hand-written JSON Schema describing schedulerd.toml to stdout.
//
// The schema is intentionally not auto-derived from the RootConfig struct
// tags — keeping it hand-written means the descriptions stay human-readable
// and the property names map 1:1 to the toml struct tags in
// internal/config/config.go (RootConfig). It documents every TOML key, its
// type, default value, and a short description so operators (and IDE
// plugins) get first-class hints.
//
// Run via: schedulerd schema

import (
	"encoding/json"
	"fmt"
	"io"
)

// schemaSubcommand emits a JSON Schema describing the schedulerd.toml
// structure to w and returns any encode error. The schema is a literal
// map[string]any tree — there is no reflection involved.
func schemaSubcommand(w io.Writer) error {
	schema := map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"$id":                  "https://coding-herms/scheduler/schedulerd.toml.schema.json",
		"title":                "schedulerd",
		"description":          "coding-hermes scheduler daemon configuration (FEAT-005 three-layer config).",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"daemon": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"db_path": map[string]any{
						"type":        "string",
						"default":     "~/.hermes/coding-hermes/scheduler.db",
						"description": "SQLite database path. ~ and ${VAR} are expanded by the caller.",
					},
					"listen": map[string]any{
						"type":        "string",
						"default":     "127.0.0.1:9090",
						"description": "HTTP listen address for the dashboard/API/MCP server (host:port).",
					},
				},
			},
			"scheduler": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"min_interval":   durationSchema("Fastest tick interval."),
					"max_interval":   durationSchema("Slowest tick interval."),
					"num_levels":     intSchema("Number of priority levels.", 2, 10),
					"weight_budget":  intSchema("Total scheduling weight budget per cycle.", 1, 100),
					"max_concurrent": intSchema("Max concurrent foreman ticks.", 1, 8),
					"tick_timeout":   durationSchema("Maximum tick wall-clock duration before kill."),
					"namespace_mode": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "Enable multi-namespace scheduling (reserved/hard_cap per namespace).",
					},
				},
			},
			"gateway": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"default":     "http://127.0.0.1:8642",
						"description": "Hermes gateway HTTP API URL. Empty falls back to exec.Command.",
					},
					"key": map[string]any{
						"type":        "string",
						"description": "Hermes gateway API key. Supports ${VAR} env-var interpolation at load time.",
					},
					"foreman_home": map[string]any{
						"type":        "string",
						"default":     "~/.hermes/foreman",
						"description": "HERMES_HOME path for foreman sessions.",
					},
				},
			},
			"duckbrain": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"namespace": map[string]any{
						"type":        "string",
						"default":     "coding-hermes",
						"description": "DuckBrain memory namespace for context sync.",
					},
					"url": map[string]any{
						"type":        "string",
						"default":     "http://localhost:3000",
						"description": "DuckBrain HTTP server URL.",
					},
				},
			},
			"projects":   projectsSchema(),
			"namespaces": namespacesSchema(),
		},
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return fmt.Errorf("encode schema: %w", err)
	}
	return nil
}

// durationSchema is a small helper that produces the JSON Schema fragment
// for a Go time.ParseDuration-style string field with a human description.
func durationSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc + " Go time.ParseDuration syntax (e.g. \"20m\", \"24h\", \"2h\").",
	}
}

// intSchema returns a JSON Schema fragment for an integer field with the
// given minimum bound and default value.
func intSchema(desc string, minimum, def int) map[string]any {
	return map[string]any{
		"type":        "integer",
		"minimum":     minimum,
		"default":     def,
		"description": desc,
	}
}

// stringSchema returns a JSON Schema fragment for a free-form string field.
func stringSchema(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// projectsSchema returns the JSON Schema fragment for the [[projects]]
// array-of-tables section.
func projectsSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Declarative project definitions (create-only upsert at startup).",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"name"},
			"properties": map[string]any{
				"name":         stringSchema("Unique project identifier."),
				"repo_url":     stringSchema("Git repository URL."),
				"workdir":      stringSchema("Absolute path to the project repo."),
				"weight":       intSchema("Concurrency budget consumed per tick.", 1, 10),
				"priority":     intSchema("Scheduling priority (1=slowest, 10=fastest).", 1, 5),
				"cooldown_s":   intSchema("Minimum seconds between ticks.", 0, 900),
				"decay_rate":   map[string]any{"type": "number", "default": 1.0, "description": "Priority decay rate."},
				"model":        stringSchema("LLM model for foreman ticks."),
				"provider":     stringSchema("Provider id."),
				"command":      stringSchema("Custom spawn command (overrides gateway)."),
				"namespace_id": stringSchema("Optional FK → namespaces.id."),
				"deliver":      stringSchema("Delivery target (platform:chat_id:thread_id)."),
				"enabled":      map[string]any{"type": "boolean", "default": true},
			},
		},
	}
}

// namespacesSchema returns the JSON Schema fragment for the [[namespaces]]
// array-of-tables section.
func namespacesSchema() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Declarative namespace definitions (create-only upsert at startup).",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"id"},
			"properties": map[string]any{
				"id":          stringSchema("Unique namespace identifier."),
				"weight":      intSchema("Total weight budget ceiling for this namespace.", 1, 10),
				"reserved":    intSchema("Guaranteed slots (always allocated).", 0, 1),
				"hard_cap":    intSchema("Absolute ceiling, never exceeded.", 0, 100),
				"description": stringSchema("Human-readable description."),
				"enabled":     map[string]any{"type": "boolean", "default": true},
			},
		},
	}
}
