package resource

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	appjson "github.com/wklken/apisix-go/pkg/json"
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

func TestRouteUnmarshalPreservesScriptPresence(t *testing.T) {
	var route Route
	if err := json.Unmarshal([]byte(`{
		"id": "script-route",
		"script": "return true"
	}`), &route); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if got, want := string(route.Script), `"return true"`; got != want {
		t.Fatalf("Route.Script = %q, want raw JSON %q", got, want)
	}
}

func TestRouteUnmarshalPreservesVarsPresence(t *testing.T) {
	var route Route
	payload := []byte(`{
		"id": "vars-route",
		"uri": "/vars",
		"vars": [["http_user", "==", "ios"]]
	}`)
	if err := appjson.Unmarshal(payload, &route); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(bytes.TrimSpace(route.Vars)) == 0 {
		t.Fatal("Route.Vars is empty, want preserved raw JSON")
	}
	if !bytes.Contains(route.Vars, []byte("http_user")) {
		t.Fatalf("Route.Vars = %s, want http_user clause", route.Vars)
	}
}

func TestRouteUnmarshalPreservesNestedVarsThatAreNotStringTriples(t *testing.T) {
	var route Route
	payload := []byte(`{"id":"nested-vars","uri":"/nested","vars":[["arg_age","==",18]]}`)
	if err := appjson.Unmarshal(payload, &route); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, want nested vars retained as raw JSON", err)
	}
	if !bytes.Contains(route.Vars, []byte("18")) {
		t.Fatalf("Route.Vars = %s, want numeric operand preserved", route.Vars)
	}
}

func TestRouteUnmarshalPreservesRemoteAddr(t *testing.T) {
	var route Route
	if err := appjson.Unmarshal(
		[]byte(`{"id":"single-remote","uri":"/single-remote","remote_addr":"10.0.0.1"}`),
		&route,
	); err != nil {
		t.Fatalf("unmarshal route: %v", err)
	}

	encoded, err := appjson.Marshal(route)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"remote_addr":"10.0.0.1"`)) {
		t.Fatalf("expected remote_addr to survive decoding, got %s", encoded)
	}
}

func TestRouteUnmarshalRejectsNullStatus(t *testing.T) {
	var route Route
	err := appjson.Unmarshal([]byte(`{"id":"null-status","uri":"/null-status","status":null}`), &route)
	if err == nil {
		t.Fatal("expected null status to be rejected")
	}
	if !strings.Contains(err.Error(), "status") {
		t.Fatalf("expected status error, got %v", err)
	}
}

func TestRouteUnmarshalDistinguishesOmittedAndExplicitStatus(t *testing.T) {
	var omitted Route
	if err := appjson.Unmarshal([]byte(`{"id":"omitted","uri":"/omitted"}`), &omitted); err != nil {
		t.Fatalf("omitted status unmarshal error = %v", err)
	}
	if omitted.StatusConfigured() || omitted.Disabled() {
		t.Fatalf(
			"omitted status configured=%v disabled=%v, want both false",
			omitted.StatusConfigured(),
			omitted.Disabled(),
		)
	}

	var enabled Route
	if err := appjson.Unmarshal([]byte(`{"id":"enabled","uri":"/enabled","status":1}`), &enabled); err != nil {
		t.Fatalf("status=1 unmarshal error = %v", err)
	}
	if !enabled.StatusConfigured() || enabled.Disabled() || enabled.Status != 1 {
		t.Fatalf(
			"status=1 configured=%v disabled=%v status=%d",
			enabled.StatusConfigured(),
			enabled.Disabled(),
			enabled.Status,
		)
	}

	var disabled Route
	if err := appjson.Unmarshal([]byte(`{"id":"disabled","uri":"/disabled","status":0}`), &disabled); err != nil {
		t.Fatalf("status=0 unmarshal error = %v", err)
	}
	if !disabled.StatusConfigured() || !disabled.Disabled() || disabled.Status != 0 {
		t.Fatalf(
			"status=0 configured=%v disabled=%v status=%d",
			disabled.StatusConfigured(),
			disabled.Disabled(),
			disabled.Status,
		)
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

func TestUpstreamUnmarshalSortsMapNodesByAddress(t *testing.T) {
	const config = `{"nodes":{"z.example.com:8080":1,"a.example.com:8080":1,"m.example.com:8080":1}}`
	const want = "a.example.com,m.example.com,z.example.com"

	for range 20 {
		var upstream Upstream
		if err := json.Unmarshal([]byte(config), &upstream); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		hosts := make([]string, 0, len(upstream.Nodes))
		for _, node := range upstream.Nodes {
			hosts = append(hosts, node.Host)
		}
		if got := strings.Join(hosts, ","); got != want {
			t.Fatalf("upstream node order = %q, want %q", got, want)
		}
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

func TestUpstreamUnmarshalAcceptsListNodeList(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"nodes": [
			{"host": "a.example.test", "port": 8080, "weight": 3},
			{"host": "b.example.test", "port": 8081, "weight": 0}
		]
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(upstream.Nodes) != 2 {
		t.Fatalf("upstream.Nodes = %#v, want two nodes", upstream.Nodes)
	}
	if upstream.Nodes[0].Host != "a.example.test" || upstream.Nodes[0].Port != 8080 ||
		upstream.Nodes[0].Weight != 3 || !upstream.Nodes[0].WeightConfigured() {
		t.Fatalf("first node = %#v, want host/port/weight configured", upstream.Nodes[0])
	}
	if upstream.Nodes[1].Host != "b.example.test" || upstream.Nodes[1].Port != 8081 ||
		upstream.Nodes[1].Weight != 0 || !upstream.Nodes[1].WeightConfigured() {
		t.Fatalf("second node = %#v, want explicit zero weight preserved", upstream.Nodes[1])
	}
}

func TestUpstreamUnmarshalAcceptsEmptyNodes(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{"nodes": []}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.Nodes == nil || len(upstream.Nodes) != 0 {
		t.Fatalf("upstream.Nodes = %#v, want empty non-nil list", upstream.Nodes)
	}
}

func TestUpstreamRoundTripPreservesDocument(t *testing.T) {
	var upstream Upstream
	const config = `{
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
	}`
	if err := json.Unmarshal([]byte(config), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	roundTripped, err := json.Marshal(&upstream)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded Upstream
	if err := json.Unmarshal(roundTripped, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() round-trip error = %v", err)
	}
	if !reflect.DeepEqual(decoded, upstream) {
		t.Fatalf("round-trip upstream = %#v, want %#v", decoded, upstream)
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
		{
			name:           "positive weight",
			json:           `{"host":"httpbin.example.test","port":80,"weight":5}`,
			wantWeight:     5,
			wantConfigured: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var node Node
			if err := json.Unmarshal([]byte(test.json), &node); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if node.Weight != test.wantWeight || node.WeightConfigured() != test.wantConfigured {
				t.Fatalf(
					"weight/configured = %d/%t, want %d/%t",
					node.Weight,
					node.WeightConfigured(),
					test.wantWeight,
					test.wantConfigured,
				)
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

func TestUpstreamUnmarshalPreservesDiscoveryFieldsAndRoundTrip(t *testing.T) {
	const config = `{
		"discovery_type": "dns",
		"service_name": "orders.default.svc.cluster.local",
		"nodes": {"127.0.0.1:8080": 1}
	}`

	var upstream Upstream
	if err := json.Unmarshal([]byte(config), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.DiscoveryType != "dns" || upstream.ServiceName != "orders.default.svc.cluster.local" {
		t.Fatalf("discovery fields = %q/%q, want dns/service name", upstream.DiscoveryType, upstream.ServiceName)
	}
	roundTripped, err := json.Marshal(&upstream)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded Upstream
	if err := json.Unmarshal(roundTripped, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() round-trip error = %v", err)
	}
	if !reflect.DeepEqual(decoded, upstream) {
		t.Fatalf("round-trip upstream = %#v, want %#v", decoded, upstream)
	}
}

func TestUpstreamUnmarshalAcceptsDiscoveryOnlyDocument(t *testing.T) {
	var upstream Upstream
	if err := json.Unmarshal([]byte(`{
		"discovery_type": "dns",
		"service_name": "orders.default.svc.cluster.local"
	}`), &upstream); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if upstream.Nodes != nil {
		t.Fatalf("discovery-only upstream nodes = %#v, want nil", upstream.Nodes)
	}
	if upstream.DiscoveryType != "dns" || upstream.ServiceName != "orders.default.svc.cluster.local" {
		t.Fatalf("discovery fields = %q/%q, want dns/service name", upstream.DiscoveryType, upstream.ServiceName)
	}
}

func TestRouteAndServiceUnmarshalWebsocketIntent(t *testing.T) {
	var route Route
	if err := json.Unmarshal([]byte(`{
		"id": "websocket-route",
		"uri": "/websocket",
		"enable_websocket": true,
		"service_id": "websocket-service"
	}`), &route); err != nil {
		t.Fatalf("route json.Unmarshal() error = %v", err)
	}
	if !route.EnableWebsocket || route.ServiceID != "websocket-service" {
		t.Fatalf("route websocket/service = %t/%q, want true/websocket-service", route.EnableWebsocket, route.ServiceID)
	}

	var service Service
	if err := json.Unmarshal([]byte(`{
		"id": "websocket-service",
		"name": "orders",
		"enable_websocket": true
	}`), &service); err != nil {
		t.Fatalf("service json.Unmarshal() error = %v", err)
	}
	if !service.EnableWebsocket || service.Name != "orders" {
		t.Fatalf("service websocket/name = %t/%q, want true/orders", service.EnableWebsocket, service.Name)
	}
}

func TestUpstreamUnmarshalRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name string
		json string
		frag string
	}{
		{name: "nodes", json: `{"nodes": 123}`, frag: "unmarshal field `nodes` fail"},
		{name: "missing nodes", json: `{}`, frag: "unmarshal field `nodes` fail"},
		{name: "type", json: `{"nodes": {}, "type": 123}`, frag: "unmarshal field `type` fail"},
		{name: "timeout", json: `{"nodes": {}, "timeout": "soon"}`, frag: "unmarshal field `timeout` fail"},
		{name: "tls", json: `{"nodes": {}, "tls": 5}`, frag: "unmarshal field `tls` fail"},
		{name: "retries", json: `{"nodes": {}, "retries": "many"}`, frag: "unmarshal field `retries` fail"},
		{name: "checks", json: `{"nodes": {}, "checks": 123}`, frag: "unmarshal field `checks` fail"},
		{name: "name", json: `{"nodes": {}, "name": 5}`, frag: "unmarshal field `name` fail"},
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
