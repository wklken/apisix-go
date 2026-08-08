package util

import (
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// CompiledSchema is a schema compiled once at plugin or configuration
// initialization and reused for every validation.
type CompiledSchema struct {
	schema *jsonschema.Schema
}

// CompileSchema compiles schema for later validation. Compile at
// initialization time, not per request.
func CompileSchema(schema string) (*CompiledSchema, error) {
	sch, err := jsonschema.CompileString("schema.json", schema)
	if err != nil {
		return nil, fmt.Errorf("compile json schema fail: %w", err)
	}
	return &CompiledSchema{schema: sch}, nil
}

// Validate checks config against the compiled schema.
func (c *CompiledSchema) Validate(config any) error {
	if err := c.schema.Validate(config); err != nil {
		return fmt.Errorf("validate fail: %w", err)
	}
	return nil
}

// Validate compiles and validates in one step. Prefer CompileSchema at
// initialization when the same schema is validated more than once.
func Validate(config any, schema string) error {
	compiled, err := CompileSchema(schema)
	if err != nil {
		return err
	}
	return compiled.Validate(config)
}
