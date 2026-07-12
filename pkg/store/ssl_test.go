package store

import (
	"testing"
)

func TestParseSSL(t *testing.T) {
	ssl, err := ParseSSL([]byte(`{
		"id": "ssl-1",
		"cert": "CERT",
		"key": "KEY"
	}`))
	if err != nil {
		t.Fatalf("ParseSSL() error = %v", err)
	}
	if ssl.ID != "ssl-1" || ssl.Cert != "CERT" || ssl.Key != "KEY" {
		t.Fatalf("ssl = %#v, want id/cert/key preserved", ssl)
	}
}

func TestGetSSLReturnsNotFoundForMissingResource(t *testing.T) {
	if _, err := GetSSL("missing"); err != ErrNotFound {
		t.Fatalf("GetSSL() error = %v, want %v", err, ErrNotFound)
	}
}
