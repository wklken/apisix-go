package resource

import (
	"encoding/json"
	"testing"
)

func TestUpstreamUnmarshalPreservesKafkaTLSOptions(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": {"kafka.example.com:9093": 1},
		"scheme": "kafka",
		"tls": {
			"verify": true,
			"client_cert": "CERT",
			"client_key": "KEY"
		}
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.TLS == nil {
		t.Fatal("upstream.TLS = nil, want parsed TLS options")
	}
	if !upstream.TLS.Verify || upstream.TLS.ClientCert != "CERT" || upstream.TLS.ClientKey != "KEY" {
		t.Fatalf("upstream.TLS = %#v, want verify/cert/key", upstream.TLS)
	}
}

func TestUpstreamUnmarshalPreservesKafkaTLSClientCertID(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": {"kafka.example.com:9093": 1},
		"scheme": "kafka",
		"tls": {"client_cert_id": 17}
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.TLS == nil || upstream.TLS.ClientCertID == nil {
		t.Fatalf("upstream.TLS = %#v, want client_cert_id preserved", upstream.TLS)
	}
}

func TestUpstreamUnmarshalParsesBracketedIPv6Node(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": {"[2001:db8::1]:8080": 1}
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(upstream.Nodes) != 1 {
		t.Fatalf("upstream.Nodes = %#v, want one node", upstream.Nodes)
	}
	if upstream.Nodes[0].Host != "[2001:db8::1]:8080" || upstream.Nodes[0].Port != 8080 {
		t.Fatalf("upstream node = %#v, want original bracketed host and port 8080", upstream.Nodes[0])
	}
}
