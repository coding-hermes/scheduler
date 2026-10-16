// Package executionschema exposes the versioned execution contract resources.
package executionschema

import "embed"

// FS contains the checked-in JSON Schema resources.
//
//go:embed v1/*.schema.json
var FS embed.FS
