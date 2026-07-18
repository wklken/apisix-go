package store

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

func TestConsumerKVAddValidatesJWEDecryptConsumerConfig(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{
			name:  "valid consumer",
			value: `{"username":"jwe-valid","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":"12345678901234567890123456789012"}}}`,
		},
		{
			name:    "key must be a string",
			value:   `{"username":"jwe-key-number","plugins":{"jwe-decrypt":{"key":123,"secret":"12345678901234567890123456789012"}}}`,
			wantErr: "jwe-decrypt consumer key must be a string",
		},
		{
			name:    "key is required",
			value:   `{"username":"jwe-key-missing","plugins":{"jwe-decrypt":{"secret":"12345678901234567890123456789012"}}}`,
			wantErr: "jwe-decrypt consumer key must be a string",
		},
		{
			name:    "key cannot be null",
			value:   `{"username":"jwe-key-null","plugins":{"jwe-decrypt":{"key":null,"secret":"12345678901234567890123456789012"}}}`,
			wantErr: "jwe-decrypt consumer key must be a string",
		},
		{
			name:    "secret must be a string",
			value:   `{"username":"jwe-secret-number","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":123}}}`,
			wantErr: "jwe-decrypt consumer secret must be a string",
		},
		{
			name:    "secret is required",
			value:   `{"username":"jwe-secret-missing","plugins":{"jwe-decrypt":{"key":"jwe-key"}}}`,
			wantErr: "jwe-decrypt consumer secret must be a string",
		},
		{
			name:    "secret cannot be null",
			value:   `{"username":"jwe-secret-null","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":null}}}`,
			wantErr: "jwe-decrypt consumer secret must be a string",
		},
		{
			name:    "raw secret must be exactly 32 characters",
			value:   `{"username":"jwe-long-raw","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":"123456789012345678901234567890123"}}}`,
			wantErr: "the secret length should be 32 chars",
		},
		{
			name:    "base64 secret must decode to 32 characters",
			value:   `{"username":"jwe-long-base64","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWZn","is_base64_encoded":true}}}`,
			wantErr: "the secret length after base64 decode should be 32 chars",
		},
		{
			name:    "base64 secret must decode",
			value:   `{"username":"jwe-bad-base64","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":"%%%%","is_base64_encoded":true}}}`,
			wantErr: "jwe-decrypt consumer secret base64 decode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			consumerStore := &Store{
				consumerKV:     make(map[string][]byte),
				consumerToKeys: make(map[string][]string),
			}
			err := consumerStore.consumerKVAdd([]byte(tt.name), []byte(tt.value))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("consumerKVAdd() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("consumerKVAdd() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConsumerKVAddDoesNotPartiallyIndexMalformedJWEDecryptConsumer(t *testing.T) {
	invalidConfigs := []struct {
		name    string
		jwe     string
		wantErr string
	}{
		{"missing key", `{"secret":"12345678901234567890123456789012"}`, "key must be a string"},
		{"null key", `{"key":null,"secret":"12345678901234567890123456789012"}`, "key must be a string"},
		{"wrong key type", `{"key":123,"secret":"12345678901234567890123456789012"}`, "key must be a string"},
		{"missing secret", `{"key":"jwe-key"}`, "secret must be a string"},
		{"null secret", `{"key":"jwe-key","secret":null}`, "secret must be a string"},
		{"wrong secret type", `{"key":"jwe-key","secret":123}`, "secret must be a string"},
		{
			"bad raw secret length",
			`{"key":"jwe-key","secret":"123456789012345678901234567890123"}`,
			"secret length should be 32 chars",
		},
		{
			"bad base64 secret length",
			`{"key":"jwe-key","secret":"YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXphYmNkZWZn","is_base64_encoded":true}`,
			"secret length after base64 decode should be 32 chars",
		},
		{
			"malformed base64 secret",
			`{"key":"jwe-key","secret":"%%%%","is_base64_encoded":true}`,
			"secret base64 decode",
		},
	}

	for _, tt := range invalidConfigs {
		t.Run(tt.name, func(t *testing.T) {
			consumerStore := &Store{
				consumerKV:     make(map[string][]byte),
				consumerToKeys: make(map[string][]string),
			}
			initial := []byte(`{"username":"alice","plugins":{"key-auth":{"key":"old-key"}}}`)
			if err := consumerStore.consumerKVAdd([]byte("alice"), initial); err != nil {
				t.Fatalf("seed consumerKVAdd() error = %v", err)
			}

			updated := []byte(
				`{"username":"alice","plugins":{"key-auth":{"key":"replacement-key"},"jwe-decrypt":` + tt.jwe + `}}`,
			)
			err := consumerStore.consumerKVAdd([]byte("alice"), updated)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("consumerKVAdd() error = %v, want %q", err, tt.wantErr)
			}
			if got := string(consumerStore.consumerKV["key-auth:old-key"]); got != "alice" {
				t.Fatalf("old key-auth index = %q, want alice", got)
			}
			if got := strings.Join(consumerStore.consumerToKeys["alice"], ","); got != "key-auth:old-key" {
				t.Fatalf("consumerToKeys[alice] = %q, want old key-auth index", got)
			}
			if _, ok := consumerStore.consumerKV["key-auth:replacement-key"]; ok {
				t.Fatal("replacement key-auth index was added for a rejected consumer")
			}
			if _, ok := consumerStore.consumerKV["jwe-decrypt:jwe-key"]; ok {
				t.Fatal("jwe-decrypt index was added for a rejected consumer")
			}
		})
	}
}

func TestConsumerKVAddAcceptsJWEDecryptRawURLAndStandardBase64Secrets(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz123456")
	tests := []struct {
		name   string
		secret string
		base64 bool
	}{
		{name: "raw", secret: string(secret)},
		{name: "raw URL base64", secret: base64.RawURLEncoding.EncodeToString(secret), base64: true},
		{name: "standard base64", secret: base64.StdEncoding.EncodeToString(secret), base64: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			consumerStore := &Store{consumerKV: make(map[string][]byte), consumerToKeys: make(map[string][]string)}
			value := []byte(
				`{"username":"alice","plugins":{"jwe-decrypt":{"key":"` + tt.name + `","secret":"` + tt.secret +
					`","is_base64_encoded":` + strconv.FormatBool(tt.base64) + `}}}`,
			)
			if err := consumerStore.consumerKVAdd([]byte("alice"), value); err != nil {
				t.Fatalf("consumerKVAdd() error = %v", err)
			}
			if got := string(consumerStore.consumerKV["jwe-decrypt:"+tt.name]); got != "alice" {
				t.Fatalf("jwe-decrypt index = %q, want alice", got)
			}
		})
	}
}
