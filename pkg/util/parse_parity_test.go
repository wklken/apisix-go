package util

import "testing"

type opsSharedTaggedConfig struct {
	HideCredentials *bool `json:"hide_credentials"`
}

func TestParityParseDoesNotAcceptGoFieldAlias(t *testing.T) {
	schema, err := CompileSchema(`{
		"type": "object",
		"properties": {
			"hide_credentials": {"type": "boolean", "default": false}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	source := map[string]any{"HideCredentials": true}
	if err := schema.Validate(source); err != nil {
		t.Fatalf("APISIX-style schema permits the unknown property: %v", err)
	}
	var config opsSharedTaggedConfig
	if err := Parse(source, &config); err != nil {
		t.Fatal(err)
	}
	if config.HideCredentials != nil {
		t.Fatalf(
			"HideCredentials = %v, want nil: a Go field name must not alias json tag hide_credentials",
			*config.HideCredentials,
		)
	}
}
