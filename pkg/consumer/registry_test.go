package consumer

import (
	"reflect"
	"strings"
	"testing"
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
