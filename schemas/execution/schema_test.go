package executionschema

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAllSchemaResourcesCompileAndAreClosed(t *testing.T) {
	entries, err := fs.Glob(FS, "v1/*.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 {
		t.Fatalf("schema count = %d, want 7", len(entries))
	}
	for _, name := range entries {
		t.Run(name, func(t *testing.T) {
			data, err := FS.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var document any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			compiler.DefaultDraft(jsonschema.Draft2020)
			compiler.AssertFormat()
			const resource = "https://example.invalid/schema.json"
			if err := compiler.AddResource(resource, document); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(resource)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(map[string]any{"credential": "must-not-fit"}); err == nil {
				t.Fatal("closed schema accepted an unknown credential member")
			}
			if strings.Contains(string(data), "password") || strings.Contains(string(data), "token\"") || strings.Contains(string(data), "secret\"") {
				t.Fatal("schema exposes a credential-shaped member")
			}
		})
	}
}

func TestExecutionDispatchDoesNotReplaceWorkDispatch(t *testing.T) {
	data, err := FS.ReadFile("v1/execution-dispatch.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"const":"execution.dispatch"`) {
		t.Fatal("typed envelope must use the separate execution.dispatch method")
	}
	if strings.Contains(string(data), `"const":"work.dispatch"`) {
		t.Fatal("typed envelope must not redefine legacy work.dispatch")
	}
}
