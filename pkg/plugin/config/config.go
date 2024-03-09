package plugin

import (
	"encoding/json"
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

func Parse(source interface{}, dest interface{}) error {
	j, err := json.Marshal(source)
	if err != nil {
		return err
	}

	err = json.Unmarshal(j, dest)
	if err != nil {
		return err
	}
	return nil
}
