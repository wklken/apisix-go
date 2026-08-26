package consumer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestFactoriesAreDeterministicAndDefensive(t *testing.T) {
	want := []string{"basic-auth", "hmac-auth", "jwe-decrypt", "jwt-auth", "key-auth", "ldap-auth", "wolf-rbac"}
	got := Factories()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Factories() = %#v, want %#v", got, want)
	}
	got[0] = "mutated"
	if again := Factories(); !reflect.DeepEqual(again, want) {
		t.Fatalf("Factories() exposed mutable registry state: %#v", again)
	}
	for _, factory := range want {
		if !Supports(factory) {
			t.Errorf("Supports(%q) = false", factory)
		}
	}
	if Supports("unknown") {
		t.Error("Supports(unknown) = true")
	}
}

func TestValidateResolvedPreservesConsumerSchemaBehavior(t *testing.T) {
	tests := []struct {
		name       string
		factory    string
		config     any
		diagnostic string
	}{
		{name: "key auth", factory: "key-auth", config: map[string]any{"key": "key", "extra": 1}},
		{name: "basic auth", factory: "basic-auth", config: map[string]any{"username": "u", "password": "p"}},
		{name: "jwt auth", factory: "jwt-auth", config: map[string]any{"key": "k", "algorithm": "RS256"}},
		{name: "hmac auth", factory: "hmac-auth", config: map[string]any{"key_id": "k", "secret_key": "s"}},
		{name: "ldap auth", factory: "ldap-auth", config: map[string]any{"user_dn": "cn=user"}},
		{
			name: "wolf config", factory: "wolf-rbac",
			config: map[string]any{
				"appid": "app", "header_prefix": "X-", "wolf_url": "http://wolf",
			},
		},
		{
			name: "jwe raw", factory: "jwe-decrypt",
			config: map[string]any{"key": "kid", "secret": "12345678901234567890123456789012"},
		},
		{
			name: "jwe environment key reference", factory: "jwe-decrypt",
			config: map[string]any{"key": "$ENV://JWE_KEY", "secret": "12345678901234567890123456789012"},
		},
		{
			name: "jwe managed key reference", factory: "jwe-decrypt",
			config: map[string]any{"key": "$secret://vault/key", "secret": "12345678901234567890123456789012"},
		},
		{
			name: "jwe key type", factory: "jwe-decrypt",
			config:     map[string]any{"key": 1, "secret": "12345678901234567890123456789012"},
			diagnostic: "jwe-decrypt consumer key must be a string",
		},
		{
			name: "jwe environment secret remains raw", factory: "jwe-decrypt",
			config:     map[string]any{"key": "kid", "secret": "$ENV://JWE_SECRET"},
			diagnostic: "the secret length should be 32 chars",
		},
		{
			name: "jwe managed secret remains raw", factory: "jwe-decrypt",
			config:     map[string]any{"key": "kid", "secret": "$secret://vault/secret"},
			diagnostic: "the secret length should be 32 chars",
		},
		{name: "unsupported", factory: "unknown", config: map[string]any{}, diagnostic: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResolved(test.factory, test.config)
			if test.diagnostic == "" {
				if err != nil {
					t.Fatalf("ValidateResolved() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.diagnostic) {
				t.Fatalf("ValidateResolved() error = %v, want %q", err, test.diagnostic)
			}
		})
	}
}

func TestLookupKeyPreservesAllConsumerFactories(t *testing.T) {
	tests := []struct {
		factory string
		config  any
		want    string
	}{
		{factory: "key-auth", config: map[string]any{"key": "key"}, want: "key"},
		{factory: "basic-auth", config: map[string]any{"username": "alice"}, want: "alice"},
		{factory: "jwt-auth", config: map[string]any{"key": "jwt"}, want: "jwt"},
		{factory: "hmac-auth", config: map[string]any{"key_id": "hmac"}, want: "hmac"},
		{factory: "ldap-auth", config: map[string]any{"user_dn": "cn=bob"}, want: "cn=bob"},
		{factory: "jwe-decrypt", config: map[string]any{"key": "jwe"}, want: "jwe"},
		{factory: "wolf-rbac", config: map[string]any{"appid": "wolf"}, want: "wolf"},
	}
	for _, test := range tests {
		t.Run(test.factory, func(t *testing.T) {
			got, err := LookupKey(test.factory, test.config)
			if err != nil || got != test.want {
				t.Fatalf("LookupKey() = %q/%v, want %q", got, err, test.want)
			}
		})
	}
	if _, err := LookupKey("unknown", map[string]any{}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("LookupKey(unknown) error = %v", err)
	}
}

func TestSchemaWitnessForFactoryRejectsUnknown(t *testing.T) {
	witness, ok := SchemaWitnessForFactory("unknown")
	if ok || witness != (SchemaWitness{}) {
		t.Fatalf("SchemaWitnessForFactory(unknown) = %#v/%v", witness, ok)
	}
}

func TestSchemaWitnessForFactoryCoversRegistry(t *testing.T) {
	for _, factory := range Factories() {
		witness, ok := SchemaWitnessForFactory(factory)
		if !ok {
			t.Fatalf("SchemaWitnessForFactory(%q) not found", factory)
		}
		if witness.Factory != factory {
			t.Fatalf("SchemaWitnessForFactory(%q).Factory = %q", factory, witness.Factory)
		}
		if witness.Schema == "" {
			t.Fatalf("SchemaWitnessForFactory(%q).Schema is empty", factory)
		}
		if _, err := util.CompileSchema(witness.Schema); err != nil {
			t.Fatalf("compile %q consumer schema: %v", factory, err)
		}

		witness.Schema = "mutated"
		again, ok := SchemaWitnessForFactory(factory)
		if !ok || again.Schema == "mutated" {
			t.Fatalf("SchemaWitnessForFactory(%q) exposed mutable registry state", factory)
		}
	}
}

func TestSchemaWitnessRegularSchemaRejectsInvalidTypes(t *testing.T) {
	witness, ok := SchemaWitnessForFactory("key-auth")
	if !ok {
		t.Fatal("key-auth schema witness not found")
	}
	compiled, err := util.CompileSchema(witness.Schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(map[string]any{"key": 1}); err == nil {
		t.Fatal("key-auth raw schema accepted a non-string key")
	}
}

func TestJWEDecryptSchemaWitnessIsStructuralOnly(t *testing.T) {
	witness, ok := SchemaWitnessForFactory("jwe-decrypt")
	if !ok {
		t.Fatal("jwe-decrypt schema witness not found")
	}
	compiled, err := util.CompileSchema(witness.Schema)
	if err != nil {
		t.Fatal(err)
	}

	accepted := []map[string]any{
		{"key": "", "secret": ""},
		{"key": "$ENV://JWE_KEY", "secret": "$secret://vault/jwe/secret"},
		{"key": "kid", "secret": "$encrypted://ciphertext", "is_base64_encoded": true},
		{"key": "kid", "secret": "short"},
	}
	for _, config := range accepted {
		if err := compiled.Validate(config); err != nil {
			t.Errorf("JWE raw schema rejected %#v: %v", config, err)
		}
	}

	rejected := []map[string]any{
		{"secret": "value"},
		{"key": "kid"},
		{"key": 1, "secret": "value"},
		{"key": "kid", "secret": false},
		{"key": "kid", "secret": "value", "is_base64_encoded": "true"},
	}
	for _, config := range rejected {
		if err := compiled.Validate(config); err == nil {
			t.Errorf("JWE raw schema accepted %#v", config)
		}
	}
}

func TestJWEDecryptResolvedValidationDiagnosticsRemainStable(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{
			name:   "key type is checked first",
			config: map[string]any{"key": 1, "secret": false},
			want:   "jwe-decrypt consumer key must be a string",
		},
		{
			name:   "secret type",
			config: map[string]any{"key": "kid", "secret": false},
			want:   "jwe-decrypt consumer secret must be a string",
		},
		{
			name:   "short secret",
			config: map[string]any{"key": "kid", "secret": "short"},
			want:   "the secret length should be 32 chars",
		},
		{
			name: "invalid base64",
			config: map[string]any{
				"key": "kid", "secret": "%%%", "is_base64_encoded": true,
			},
			want: "jwe-decrypt consumer secret base64 decode: illegal base64 data at input byte 0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResolved("jwe-decrypt", test.config)
			if err == nil || err.Error() != test.want {
				t.Fatalf("ValidateResolved() error = %v, want %q", err, test.want)
			}
		})
	}
}
