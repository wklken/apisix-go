package datadog

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitSetsDatadogDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if !p.config.PreferName {
		t.Fatal("prefer_name = false, want true")
	}
	if p.metadata.Host != "127.0.0.1" {
		t.Fatalf("metadata host = %q, want 127.0.0.1", p.metadata.Host)
	}
	if p.metadata.Port != 8125 {
		t.Fatalf("metadata port = %d, want 8125", p.metadata.Port)
	}
	if p.metadata.Namespace != "apisix" {
		t.Fatalf("namespace = %q, want apisix", p.metadata.Namespace)
	}
	if len(p.metadata.ConstantTags) != 1 || p.metadata.ConstantTags[0] != "source:apisix" {
		t.Fatalf("constant tags = %v, want [source:apisix]", p.metadata.ConstantTags)
	}
	if p.config.BatchName != "datadog" || p.config.BatchMaxSize != 1000 || p.config.InactiveTimeout != 5 {
		t.Fatalf(
			"batch defaults = name:%q size:%d inactive:%d, want datadog/1000/5",
			p.config.BatchName,
			p.config.BatchMaxSize,
			p.config.InactiveTimeout,
		)
	}
}

func TestPostInitPreservesExplicitPreferNameFalse(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"prefer_name": false}, p.Config()); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.PreferName {
		t.Fatal("prefer_name = true, want explicit false")
	}
}

func TestGenerateTagsIncludesConfiguredAndRequestTags(t *testing.T) {
	p := newTestPlugin(t, Config{
		IncludePath:   true,
		IncludeMethod: true,
		ConstantTags:  []string{"route:local"},
	})
	p.metadata.ConstantTags = []string{"source:apisix"}

	tags := p.generateTags(metricEntry{
		Status: 201,
		Path:   "/orders",
		Method: http.MethodPost,
		Scheme: "http",
	})

	want := []string{
		"source:apisix",
		"route:local",
		"path:/orders",
		"method:POST",
		"response_status:201",
		"response_status_class:2xx",
		"scheme:http",
	}
	for _, tag := range want {
		if !contains(tags, tag) {
			t.Fatalf("tags = %v, want %q", tags, tag)
		}
	}
}

func TestGenerateTagsIncludesAPISIXResourceTags(t *testing.T) {
	p := newTestPlugin(t, Config{})
	p.metadata.ConstantTags = []string{"source:apisix"}

	tags := p.generateTags(metricEntry{
		RouteID:      "route-1",
		RouteName:    "orders-route",
		ServiceID:    "service-1",
		ServiceName:  "orders-service",
		ConsumerName: "alice",
		BalancerIP:   "10.0.0.9",
		Status:       200,
	})

	for _, tag := range []string{
		"route_name:orders-route",
		"service_name:orders-service",
		"consumer:alice",
		"balancer_ip:10.0.0.9",
	} {
		if !contains(tags, tag) {
			t.Fatalf("tags = %v, want %q", tags, tag)
		}
	}
}

func TestGenerateTagsPreferNameFalseUsesIDs(t *testing.T) {
	p := &Plugin{config: Config{PreferName: false, preferNameSet: true}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	tags := p.generateTags(metricEntry{
		RouteID:     "route-1",
		RouteName:   "orders-route",
		ServiceID:   "service-1",
		ServiceName: "orders-service",
		Status:      200,
	})

	for _, tag := range []string{"route_name:route-1", "service_name:service-1"} {
		if !contains(tags, tag) {
			t.Fatalf("tags = %v, want %q", tags, tag)
		}
	}
}

func TestMetricLinesUseDogStatsDFormat(t *testing.T) {
	p := newTestPlugin(t, Config{})
	p.metadata.Namespace = "apisix"
	p.metadata.ConstantTags = []string{"source:apisix"}

	lines := p.metricLines(metricEntry{
		LatencyMS:     12,
		ApisixLatency: 12,
		IngressSize:   7,
		EgressSize:    5,
		Status:        204,
	})

	want := []string{
		"apisix.request.counter:1|c|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.request.latency:12|h|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.apisix.latency:12|h|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.ingress.size:7|ms|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.egress.size:5|ms|#source:apisix,response_status:204,response_status_class:2xx",
	}
	for _, line := range want {
		if !contains(lines, line) {
			t.Fatalf("lines = %v, want %q", lines, line)
		}
	}
}

func TestApisixLatencySubtractsUpstreamLatency(t *testing.T) {
	tests := []struct {
		name     string
		total    int64
		upstream int64
		want     int64
	}{
		{
			name:     "no upstream latency keeps total latency",
			total:    120,
			upstream: 0,
			want:     120,
		},
		{
			name:     "subtracts upstream latency",
			total:    120,
			upstream: 80,
			want:     40,
		},
		{
			name:     "clamps negative values to zero",
			total:    10,
			upstream: 20,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apisixLatency(tt.total, tt.upstream)
			if got != tt.want {
				t.Fatalf("apisixLatency(%d, %d) = %d, want %d", tt.total, tt.upstream, got, tt.want)
			}
		})
	}
}

func TestSendWritesUDPMetrics(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{})
	p.metadata = Metadata{
		Host:         host,
		Port:         mustAtoi(t, port),
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}

	p.Send(metricEntry{
		LatencyMS:     1,
		ApisixLatency: 1,
		IngressSize:   2,
		EgressSize:    3,
		Status:        200,
	})

	messages := collectMessages(t, received, 5)
	if !containsPrefix(messages, "apisix.request.counter:1|c|#") {
		t.Fatalf("messages = %v, want request counter", messages)
	}
	if !containsPrefix(messages, "apisix.egress.size:3|ms|#") {
		t.Fatalf("messages = %v, want egress size", messages)
	}
}

func TestHandlerCapturesStatusAndSizes(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{IncludePath: true, IncludeMethod: true, BatchMaxSize: 1})
	p.metadata = Metadata{
		Host:         host,
		Port:         mustAtoi(t, port),
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}

	req := httptest.NewRequest(http.MethodPut, "/orders/1", strings.NewReader("request"))
	req.Header.Set("X-Forwarded-Proto", "https")
	req.ContentLength = 7
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("reply"))
	})).ServeHTTP(rr, req)

	messages := collectMessages(t, received, 5)
	if !containsLinePart(messages, "response_status:201") {
		t.Fatalf("messages = %v, want response_status tag", messages)
	}
	if !containsLinePart(messages, "path:/orders/1") {
		t.Fatalf("messages = %v, want path tag", messages)
	}
	if !containsPrefix(messages, "apisix.ingress.size:7|ms|#") {
		t.Fatalf("messages = %v, want ingress size", messages)
	}
	if !containsPrefix(messages, "apisix.egress.size:5|ms|#") {
		t.Fatalf("messages = %v, want egress size", messages)
	}
}

func TestHandlerCapturesUpstreamLatency(t *testing.T) {
	addr, received := startUDPServer(t, 6)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{BatchMaxSize: 1})
	p.metadata = Metadata{
		Host:         host,
		Port:         mustAtoi(t, port),
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}

	req := httptest.NewRequest(http.MethodGet, "/orders/1", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$upstream_latency", int64(42))

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	messages := collectMessages(t, received, 6)
	if !containsPrefix(messages, "apisix.upstream.latency:42|h|#") {
		t.Fatalf("messages = %v, want upstream latency", messages)
	}
}

func TestHandlerUsesMatchedURIForPathTag(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{IncludePath: true, BatchMaxSize: 1})
	p.metadata = Metadata{
		Host:         host,
		Port:         mustAtoi(t, port),
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}

	req := httptest.NewRequest(http.MethodGet, "/orders/123", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$matched_uri": "/orders/:id",
	})

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	messages := collectMessages(t, received, 5)
	if !containsLinePart(messages, "path:/orders/:id") {
		t.Fatalf("messages = %v, want matched URI path tag", messages)
	}
	if containsLinePart(messages, "path:/orders/123") {
		t.Fatalf("messages = %v, want no raw request path tag", messages)
	}
}

func TestHandlerCapturesAPISIXResourceTags(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{BatchMaxSize: 1})
	p.metadata = Metadata{
		Host:         host,
		Port:         mustAtoi(t, port),
		Namespace:    "apisix",
		ConstantTags: []string{"source:apisix"},
	}

	req := httptest.NewRequest(http.MethodGet, "/orders/1", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":     "route-1",
		"$route_name":   "orders-route",
		"$service_id":   "service-1",
		"$service_name": "orders-service",
		"$balancer_ip":  "10.0.0.9",
	})
	apisixctx.AttachConsumer(req, resource.Consumer{Username: "alice"})

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	messages := collectMessages(t, received, 5)
	for _, tag := range []string{
		"route_name:orders-route",
		"service_name:orders-service",
		"consumer:alice",
		"balancer_ip:10.0.0.9",
	} {
		if !containsLinePart(messages, tag) {
			t.Fatalf("messages = %v, want tag %q", messages, tag)
		}
	}
}

func TestHandlerBatchesMetricsUntilBatchMaxSize(t *testing.T) {
	addr, received := startUDPServer(t, 10)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{BatchMaxSize: 2})
	p.metadata = Metadata{Host: host, Port: mustAtoi(t, port), Namespace: "apisix"}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
	select {
	case message := <-received:
		t.Fatalf("received metric before batch filled: %q", message)
	case <-time.After(50 * time.Millisecond):
	}

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))
	messages := collectMessages(t, received, 10)
	if len(messages) != 10 {
		t.Fatalf("messages = %d, want 10 for two five-metric entries", len(messages))
	}
}

func startUDPServer(t *testing.T, count int) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
	})

	received := make(chan string, count)
	go func() {
		for range count {
			buf := make([]byte, 4096)
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func collectMessages(t *testing.T, received <-chan string, count int) []string {
	t.Helper()

	messages := make([]string, 0, count)
	for len(messages) < count {
		select {
		case message := <-received:
			messages = append(messages, message)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for UDP metrics, got %v", messages)
		}
	}
	return messages
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func containsLinePart(values []string, part string) bool {
	for _, value := range values {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
