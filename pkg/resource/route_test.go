package resource

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRouteUnmarshalPreservesLabels(t *testing.T) {
	var route Route
	if err := json.Unmarshal([]byte(`{
		"id": "labeled-route",
		"uri": "/labels",
		"labels": {"key": "testvalue", "revision": 2}
	}`), &route); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := map[string]any{"key": "testvalue", "revision": float64(2)}
	if !reflect.DeepEqual(route.Labels, want) {
		t.Fatalf("route labels = %#v, want %#v", route.Labels, want)
	}
}

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

func TestUpstreamUnmarshalDefaultsHTTPAndSplitsNodeAddress(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": {"127.0.0.1:8080": 1},
		"type": "roundrobin"
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.Scheme != "http" {
		t.Fatalf("upstream.Scheme = %q, want http", upstream.Scheme)
	}
	if len(upstream.Nodes) != 1 {
		t.Fatalf("upstream.Nodes = %#v, want one node", upstream.Nodes)
	}
	if upstream.Nodes[0].Host != "127.0.0.1" || upstream.Nodes[0].Port != 8080 {
		t.Fatalf("upstream node = %#v, want host 127.0.0.1 and port 8080", upstream.Nodes[0])
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
	if upstream.Nodes[0].Host != "2001:db8::1" || upstream.Nodes[0].Port != 8080 {
		t.Fatalf("upstream node = %#v, want IPv6 host and port 8080", upstream.Nodes[0])
	}
}

func TestUpstreamUnmarshalTracksWhetherRetriesWereConfigured(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     string
		want       int
		configured bool
	}{
		{
			name:       "omitted",
			config:     `{"nodes":{"127.0.0.1:8080":1}}`,
			want:       0,
			configured: false,
		},
		{
			name:       "explicit zero",
			config:     `{"nodes":{"127.0.0.1:8080":1},"retries":0}`,
			want:       0,
			configured: true,
		},
		{
			name:       "explicit positive",
			config:     `{"nodes":{"127.0.0.1:8080":1},"retries":2}`,
			want:       2,
			configured: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstream Upstream
			if err := json.Unmarshal([]byte(test.config), &upstream); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if upstream.Retries != test.want {
				t.Fatalf("Retries = %d, want %d", upstream.Retries, test.want)
			}
			if upstream.RetriesConfigured() != test.configured {
				t.Fatalf("RetriesConfigured() = %t, want %t", upstream.RetriesConfigured(), test.configured)
			}
		})
	}
}
