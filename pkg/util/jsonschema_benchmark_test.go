package util

import (
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// BenchmarkValidateCompiled compares the compile-per-call validation path with
// a schema compiled once at initialization. Both paths are modeled directly on
// jsonschema so the corpus stays stable across the migration; the production
// entry points behave identically to the modeled paths.

type benchmarkValidateCase struct {
	name   string
	schema string
	config any
	valid  bool
}

var benchmarkValidateCases = []benchmarkValidateCase{
	{
		name:   "small-valid",
		schema: `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`,
		config: map[string]any{"count": 2},
		valid:  true,
	},
	{
		name:   "small-invalid",
		schema: `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`,
		config: map[string]any{"count": "two"},
		valid:  false,
	},
	{
		name: "plugin-valid",
		schema: `{
			"type": "object",
			"properties": {
				"header": {"type": "string", "default": "apikey"},
				"query": {"type": "string", "default": "apikey"},
				"realm": {"type": "string", "default": "key", "minLength": 1, "maxLength": 128},
				"hide_credentials": {"type": "boolean", "default": false},
				"anonymous_consumer": {"type": "string", "minLength": 1}
			}
		}`,
		config: map[string]any{
			"header": "X-API-Key",
			"query":  "apikey",
			"realm":  "key",
		},
		valid: true,
	},
	{
		name: "plugin-invalid",
		schema: `{
			"type": "object",
			"properties": {
				"header": {"type": "string", "default": "apikey"},
				"query": {"type": "string", "default": "apikey"},
				"realm": {"type": "string", "default": "key", "minLength": 1, "maxLength": 128},
				"hide_credentials": {"type": "boolean", "default": false},
				"anonymous_consumer": {"type": "string", "minLength": 1}
			}
		}`,
		config: map[string]any{"realm": "has spaces", "anonymous_consumer": ""},
		valid:  false,
	},
}

func BenchmarkValidateCompiled(b *testing.B) {
	for _, tc := range benchmarkValidateCases {
		b.Run("compile-per-call/"+tc.name, func(b *testing.B) {
			benchmarkValidateCompilePerCall(b, tc)
		})
		b.Run("compiled/"+tc.name, func(b *testing.B) {
			benchmarkValidateCompiledOnce(b, tc)
		})
	}
}

func benchmarkValidateCompilePerCall(b *testing.B, tc benchmarkValidateCase) {
	for b.Loop() {
		schema, err := jsonschema.CompileString("schema.json", tc.schema)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkValidateExpectation(b, tc, schema)
	}
}

func benchmarkValidateCompiledOnce(b *testing.B, tc benchmarkValidateCase) {
	schema, err := jsonschema.CompileString("schema.json", tc.schema)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		benchmarkValidateExpectation(b, tc, schema)
	}
}

func benchmarkValidateExpectation(b *testing.B, tc benchmarkValidateCase, schema *jsonschema.Schema) {
	if err := schema.Validate(tc.config); err != nil {
		if tc.valid {
			b.Fatal(err)
		}
		return
	}
	if !tc.valid {
		b.Fatal("validation unexpectedly succeeded")
	}
}
