package store

import (
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
			name:    "secret must be a string",
			value:   `{"username":"jwe-secret-number","plugins":{"jwe-decrypt":{"key":"jwe-key","secret":123}}}`,
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
