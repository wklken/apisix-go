package resource

import (
	"encoding/json"
	"reflect"
	"strings"
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

func TestNodeWeightPresence(t *testing.T) {
	tests := []struct {
		name           string
		json           string
		wantWeight     int
		wantConfigured bool
	}{
		{name: "omitted weight", json: `{"host":"httpbin.example.test","port":80}`, wantConfigured: false},
		{name: "explicit zero", json: `{"host":"httpbin.example.test","port":80,"weight":0}`, wantConfigured: true},
		{name: "positive weight", json: `{"host":"httpbin.example.test","port":80,"weight":5}`, wantWeight: 5, wantConfigured: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var node Node
			if err := json.Unmarshal([]byte(test.json), &node); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if node.Weight != test.wantWeight || node.WeightConfigured() != test.wantConfigured {
				t.Fatalf("weight/configured = %d/%t, want %d/%t", node.Weight, node.WeightConfigured(), test.wantWeight, test.wantConfigured)
			}
		})
	}
}

func TestParseNodeAddress(t *testing.T) {
	tests := []struct {
		address  string
		wantHost string
		wantPort int
	}{
		{address: "example.test", wantHost: "example.test", wantPort: 80},
		{address: "example.test:8080", wantHost: "example.test", wantPort: 8080},
		{address: "[2001:db8::1]:8443", wantHost: "2001:db8::1", wantPort: 8443},
		{address: "[2001:db8::1]", wantHost: "[2001:db8::1]", wantPort: 80},
		{address: "2001:db8::1", wantHost: "2001:db8::1", wantPort: 80},
		{address: "example.test:not-a-port", wantHost: "example.test:not-a-port", wantPort: 80},
	}
	for _, test := range tests {
		host, port := parseNodeAddress(test.address)
		if host != test.wantHost || port != test.wantPort {
			t.Fatalf("parseNodeAddress(%q) = %q/%d, want %q/%d", test.address, host, port, test.wantHost, test.wantPort)
		}
	}
}

func TestUpstreamUnmarshalCompleteDocument(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"type": "roundrobin",
		"nodes": {"backend.example.test:8080": 1},
		"scheme": "https",
		"timeout": {"connect": 3, "send": 4, "read": 5},
		"tls": {"verify": true},
		"retries": 2,
		"checks": {"active": {"type": "http"}},
		"hash_on": "vars",
		"key": "remote_addr",
		"pass_host": "rewrite",
		"upstream_host": "up.example.test",
		"name": "upstream-1",
		"desc": "a complete upstream"
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.Type != "roundrobin" || upstream.Scheme != "https" {
		t.Fatalf("type/scheme = %q/%q", upstream.Type, upstream.Scheme)
	}
	if upstream.Timeout.Connect != 3 || upstream.Timeout.Send != 4 || upstream.Timeout.Read != 5 {
		t.Fatalf("timeout = %+v", upstream.Timeout)
	}
	if upstream.TLS == nil || !upstream.TLS.Verify {
		t.Fatalf("tls = %+v, want verify enabled", upstream.TLS)
	}
	if upstream.Retries != 2 || !upstream.RetriesConfigured() {
		t.Fatalf("retries = %d, configured = %t", upstream.Retries, upstream.RetriesConfigured())
	}
	if upstream.Checks == nil || upstream.HashOn != "vars" || upstream.Key != "remote_addr" {
		t.Fatalf("checks/hash_on/key = %+v/%q/%q", upstream.Checks, upstream.HashOn, upstream.Key)
	}
	if upstream.PassHost != "rewrite" || upstream.UpstreamHost != "up.example.test" {
		t.Fatalf("pass_host/upstream_host = %q/%q", upstream.PassHost, upstream.UpstreamHost)
	}
	if upstream.Name != "upstream-1" || upstream.Desc != "a complete upstream" {
		t.Fatalf("name/desc = %q/%q", upstream.Name, upstream.Desc)
	}
	if len(upstream.Nodes) != 1 || upstream.Nodes[0].Host != "backend.example.test" || upstream.Nodes[0].Port != 8080 {
		t.Fatalf("nodes = %+v", upstream.Nodes)
	}
}

func TestUpstreamUnmarshalRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name string
		json string
		frag string
	}{
		{name: "nodes", json: `{"nodes": 123}`, frag: "unmarshal field `nodes` fail"},
		{name: "type", json: `{"nodes": {}, "type": 123}`, frag: "unmarshal field `type` fail"},
		{name: "timeout", json: `{"nodes": {}, "timeout": "soon"}`, frag: "unmarshal field `timeout` fail"},
		{name: "tls", json: `{"nodes": {}, "tls": 5}`, frag: "unmarshal field `tls` fail"},
		{name: "retries", json: `{"nodes": {}, "retries": "many"}`, frag: "unmarshal field `retries` fail"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstream Upstream
			err := json.Unmarshal([]byte(test.json), &upstream)
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("json.Unmarshal() error = %v, want fragment %q", err, test.frag)
			}
		})
	}
}
