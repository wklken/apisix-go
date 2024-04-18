package util

import (
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func Validate(config interface{}, schema string) error {
	// return nil
	sch, err := jsonschema.CompileString("schema.json", schema)
	if err != nil {
		return fmt.Errorf("compile json schema fail: %w", err)
	}

	if err = sch.Validate(config); err != nil {
		return fmt.Errorf("validate fail: %w", err)
	}

	return nil
}
