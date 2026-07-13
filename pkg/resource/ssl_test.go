package resource

import (
	"encoding/json"
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
}
