package openid_connect

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityOIDCSchemaPreservesOfficialUnknownFieldAdmission(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"client_id":       "apisix",
		"client_secret":   "secret-a",
		"discovery":       "https://idp.example.test/.well-known/openid-configuration",
		"session":         map[string]any{"secret": "0123456789abcdef"},
		"extension_field": "ignored-by-official-schema",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("APISIX 3.17 admits unspecified top-level properties: %v", err)
	}
}
