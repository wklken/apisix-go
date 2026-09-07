package resource

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSSLUnmarshalPreservesClientCertificate(t *testing.T) {
	var ssl SSL
	if err := json.Unmarshal([]byte(`{
		"id": "ssl-1",
		"snis": ["kafka.example.com"],
		"cert": "CERT",
		"key": "KEY",
		"status": 1,
		"labels": {"team": "edge"}
	}`), &ssl); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if ssl.ID != "ssl-1" || ssl.Cert != "CERT" || ssl.Key != "KEY" || ssl.Status != 1 {
		t.Fatalf("ssl = %#v, want id/cert/key/status preserved", ssl)
	}
	if len(ssl.Snis) != 1 || ssl.Snis[0] != "kafka.example.com" || ssl.Labels["team"] != "edge" {
		t.Fatalf("ssl metadata = %#v, want snis and labels preserved", ssl)
	}
	if ssl.GM {
		t.Fatal("GM = true, want omitted GM default false")
	}
}

func TestSSLUnmarshalPreservesGMSignAndEncryptionPairs(t *testing.T) {
	var ssl SSL
	if err := json.Unmarshal([]byte(`{
		"id": "gm-ssl",
		"gm": true,
		"snis": ["gm.example.com"],
		"cert": "enc-cert",
		"key": "enc-key",
		"certs": ["sign-cert"],
		"keys": ["sign-key"]
	}`), &ssl); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !ssl.GM || ssl.Cert != "enc-cert" || ssl.Key != "enc-key" {
		t.Fatalf("ssl = %#v, want GM encryption pair preserved", ssl)
	}
	if len(ssl.Certs) != 1 || ssl.Certs[0] != "sign-cert" || len(ssl.Keys) != 1 || ssl.Keys[0] != "sign-key" {
		t.Fatalf("ssl extra certs/keys = %#v, want GM sign pair preserved", ssl)
	}
}

func TestSSLUnmarshalRejectsGMWithoutSignPair(t *testing.T) {
	var ssl SSL
	err := json.Unmarshal([]byte(`{
		"gm": true,
		"cert": "enc-cert",
		"key": "enc-key",
		"snis": ["gm.example.com"]
	}`), &ssl)
	if err == nil {
		t.Fatal("json.Unmarshal() error = nil, want GM sign cert/key requirement")
	}
	if !strings.Contains(err.Error(), "sign cert/key are required") {
		t.Fatalf("json.Unmarshal() error = %v, want sign cert/key requirement", err)
	}
}

func TestSSLUnmarshalRejectsGMWithoutEncryptionPair(t *testing.T) {
	var ssl SSL
	err := json.Unmarshal([]byte(`{
		"gm": true,
		"certs": ["sign-cert"],
		"keys": ["sign-key"]
	}`), &ssl)
	if err == nil {
		t.Fatal("json.Unmarshal() error = nil, want GM enc cert/key requirement")
	}
	if !strings.Contains(err.Error(), "enc cert/key are required") {
		t.Fatalf("json.Unmarshal() error = %v, want enc cert/key requirement", err)
	}
}
