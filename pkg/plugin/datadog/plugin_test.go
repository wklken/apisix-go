package datadog

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type cancellationWatchConn struct {
	net.Conn
	closeCalls   atomic.Int32
	closeStarted chan struct{}
	releaseClose <-chan struct{}
}

func (c *cancellationWatchConn) Close() error {
	c.closeCalls.Add(1)
	select {
	case c.closeStarted <- struct{}{}:
	default:
	}
	if c.releaseClose != nil {
		<-c.releaseClose
	}
	return nil
}

func TestWatchConnectionCancellation(t *testing.T) {
	for _, scenario := range []string{
		"cancellation closes connection",
		"normal completion preserves reused connection",
		"cleanup joins close already running",
		"nil context is no-op",
	} {
		t.Run(scenario, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario != "nil context is no-op" {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			var releaseClose chan struct{}
			if scenario == "cleanup joins close already running" {
				releaseClose = make(chan struct{})
			}
			conn := &cancellationWatchConn{
				closeStarted: make(chan struct{}, 1),
				releaseClose: releaseClose,
			}
			cleanup := watchConnectionCancellation(ctx, conn)

			switch scenario {
			case "cancellation closes connection":
				cancel()
				cleanup()
			case "normal completion preserves reused connection":
				cleanup()
				cancel()
			case "cleanup joins close already running":
				cancel()
				select {
				case <-conn.closeStarted:
				case <-time.After(time.Second):
					t.Fatal("cancellation callback did not enter Close")
				}
				cleanupDone := make(chan struct{})
				go func() {
					cleanup()
					close(cleanupDone)
				}()
				select {
				case <-cleanupDone:
					t.Fatal("cleanup returned while Close was blocked")
				case <-time.After(20 * time.Millisecond):
				}
				close(releaseClose)
				select {
				case <-cleanupDone:
				case <-time.After(time.Second):
					t.Fatal("cleanup did not return after Close completed")
				}
			case "nil context is no-op":
				cleanup()
			}

			wantCloseCalls := int32(0)
			if scenario == "cancellation closes connection" ||
				scenario == "cleanup joins close already running" {
				wantCloseCalls = 1
			}
			if got := conn.closeCalls.Load(); got != wantCloseCalls {
				t.Fatalf("Close calls = %d, want %d", got, wantCloseCalls)
			}
		})
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, documents map[string][]byte) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t), Metadata: mustMetadataView(t, documents)})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func mustMetadataView(t *testing.T, documents map[string][]byte) runtime.MetadataView {
	t.Helper()
	view, err := runtime.NewMetadataView(documents)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPreparedGenerationsRetainMetadataNamespace(t *testing.T) {
	n := newTestPluginWithMetadata(t, Config{}, map[string][]byte{
		name: []byte(`{"namespace":"n"}`),
	})
	n1 := newTestPluginWithMetadata(t, Config{}, map[string][]byte{
		name: []byte(`{"namespace":"n1"}`),
	})

	if got := n.metricLines(metricEntry{})[0]; !strings.HasPrefix(got, "n.") {
		t.Fatalf("N metric = %q, want n. prefix", got)
	}
	if got := n1.metricLines(metricEntry{})[0]; !strings.HasPrefix(got, "n1.") {
		t.Fatalf("N+1 metric = %q, want n1. prefix", got)
	}
}

func TestPostInitRejectsInvalidMetadataBeforeSideEffects(t *testing.T) {
	p := &Plugin{}
	p.SetDependencies(
		base.Dependencies{Tasks: newLoggerTestTaskOwner(t), Metadata: mustMetadataView(t, map[string][]byte{
			name: []byte(`{"namespace":true}`),
		})},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "datadog metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want redacted metadata decode failure", err)
	}
	if p.BatchProcessor != nil || p.conn != nil || p.config.BatchName != "" {
		t.Fatalf(
			"side effects published after invalid metadata: processor=%v conn=%v batch=%q",
			p.BatchProcessor,
			p.conn,
			p.config.BatchName,
		)
	}
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

func TestPostInitUsesRouteEndpoint(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.9", Port: 9125})

	if p.metadata.Host != "127.0.0.9" || p.metadata.Port != 9125 {
		t.Fatalf("metadata endpoint = %s:%d, want 127.0.0.9:9125", p.metadata.Host, p.metadata.Port)
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
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
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

func TestGenerateTagsMatchesDatadogStableTagOrder(t *testing.T) {
	p := newTestPlugin(t, Config{
		IncludePath:   true,
		IncludeMethod: true,
		ConstantTags:  []string{"route:local"},
	})
	p.metadata.ConstantTags = []string{"source:apisix"}

	got := p.generateTags(metricEntry{
		RouteID:     "route-1",
		RouteName:   "orders-route",
		ServiceID:   "service-1",
		ServiceName: "orders-service",
		Status:      201,
		Path:        "/orders",
		Method:      http.MethodPost,
		Scheme:      "http",
	})
	want := []string{
		"source:apisix",
		"route:local",
		"route_name:orders-route",
		"path:/orders",
		"method:POST",
		"service_name:orders-service",
		"response_status:201",
		"response_status_class:2xx",
		"scheme:http",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
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
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
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
		LatencyMS:          12,
		UpstreamLatency:    7,
		HasUpstreamLatency: true,
		ApisixLatency:      12,
		IngressSize:        7,
		EgressSize:         5,
		Status:             204,
	})

	want := []string{
		"apisix.request.counter:1|c|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.request.latency:12|h|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.upstream.latency:7|h|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.apisix.latency:12|h|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.ingress.size:7|ms|#source:apisix,response_status:204,response_status_class:2xx",
		"apisix.egress.size:5|ms|#source:apisix,response_status:204,response_status_class:2xx",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %v, want exact ordered lines %v", lines, want)
	}
}

func TestRunLogPhaseUsesCapturedHTTPWireSizes(t *testing.T) {
	delivered := make(chan metricEntry, 1)
	p := &Plugin{}
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize:      1,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
		ShutdownTimeout:   time.Second,
		DeliveryTimeout:   time.Second,
		MaxPendingEntries: 1,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]["entry"].(metricEntry)
		return 0, nil
	})
	t.Cleanup(p.Stop)

	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			ContentLength: 0,
			RequestVars: map[string]any{
				"$request_length": int64(108),
				"$bytes_sent":     int64(133),
			},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK, Bytes: 11},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		if entry.IngressSize != 108 || entry.EgressSize != 133 {
			t.Fatalf(
				"wire sizes = ingress:%d egress:%d, want request_length:108 bytes_sent:133",
				entry.IngressSize,
				entry.EgressSize,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("detached metric was not delivered")
	}
}

func TestMetricLinesOmitAbsentUpstreamLatency(t *testing.T) {
	p := newTestPlugin(t, Config{})

	lines := p.metricLines(metricEntry{})
	if len(lines) != 5 {
		t.Fatalf("metric lines = %d, want five without upstream latency", len(lines))
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "apisix.upstream.latency:") {
			t.Fatalf("metric lines include fabricated upstream latency: %v", lines)
		}
	}
}

func TestMetricLinesIncludePresentZeroUpstreamLatency(t *testing.T) {
	p := newTestPlugin(t, Config{})

	lines := p.metricLines(metricEntry{HasUpstreamLatency: true})
	if len(lines) != 6 {
		t.Fatalf("metric lines = %d, want six with present upstream latency", len(lines))
	}
	if !strings.HasPrefix(lines[2], "apisix.upstream.latency:0|h|") {
		t.Fatalf("third metric line = %q, want present zero upstream latency", lines[2])
	}
}

func TestPostInitPreservesExplicitRetryDelayZero(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"retry_delay": 0}, p.Config()); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay = %d, want explicit zero", p.config.RetryDelay)
	}
}

func TestSchemasRejectInvalidConstantTag(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}

	invalid := map[string]any{"constant_tags": []any{"1 invalid tag"}}
	if err := util.Validate(invalid, p.GetSchema()); err == nil {
		t.Fatal("route config accepted an invalid constant tag")
	}
	if err := util.Validate(invalid, p.GetMetadataSchema()); err == nil {
		t.Fatal("metadata accepted an invalid constant tag")
	}

	validMetadata := map[string]any{
		"host":          "127.0.0.1",
		"port":          8125,
		"namespace":     "apisix",
		"constant_tags": []any{"source:apisix"},
	}
	if err := util.Validate(validMetadata, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata rejected valid DogStatsD endpoint: %v", err)
	}
}

func TestMetadataSchemaPublishesPinnedDefaults(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	var document struct {
		Properties map[string]struct {
			Default any `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetMetadataSchema()), &document); err != nil {
		t.Fatalf("decode metadata schema: %v", err)
	}

	want := map[string]any{
		"host":          "127.0.0.1",
		"port":          float64(8125),
		"namespace":     "apisix",
		"constant_tags": []any{"source:apisix"},
	}
	if len(document.Properties) != len(want) {
		t.Fatalf("metadata properties = %v, want exactly %v", document.Properties, want)
	}
	for property, wantDefault := range want {
		definition, ok := document.Properties[property]
		if !ok {
			t.Fatalf("metadata schema has no %q property", property)
		}
		if !reflect.DeepEqual(definition.Default, wantDefault) {
			t.Fatalf("metadata %s default = %#v, want %#v", property, definition.Default, wantDefault)
		}
	}

	for _, tt := range []struct {
		name     string
		metadata map[string]any
		valid    bool
	}{
		{name: "empty uses defaults", metadata: map[string]any{}, valid: true},
		{name: "explicit values", metadata: map[string]any{
			"host": "dogstatsd", "port": 18125, "namespace": "custom", "constant_tags": []any{"env:test"},
		}, valid: true},
		{name: "negative port", metadata: map[string]any{"port": -1}, valid: false},
		{name: "non-string host", metadata: map[string]any{"host": 127}, valid: false},
		{name: "non-string namespace", metadata: map[string]any{"namespace": false}, valid: false},
		{name: "invalid constant tag", metadata: map[string]any{"constant_tags": []any{"1 invalid tag"}}, valid: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := util.Validate(tt.metadata, p.GetMetadataSchema())
			if tt.valid && err != nil {
				t.Fatalf("metadata validation error = %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("metadata validation unexpectedly succeeded")
			}
		})
	}
}

func TestMetadataWithDefaultsOverlaysConfiguredValues(t *testing.T) {
	got := metadataWithDefaults(Metadata{Host: "dogstatsd", Port: 18125})
	if got.Host != "dogstatsd" || got.Port != 18125 {
		t.Fatalf("configured endpoint = %s:%d", got.Host, got.Port)
	}
	if got.Namespace != "apisix" {
		t.Fatalf("default namespace = %q, want apisix", got.Namespace)
	}
	if !reflect.DeepEqual(got.ConstantTags, []string{"source:apisix"}) {
		t.Fatalf("default constant tags = %#v", got.ConstantTags)
	}
}

func TestSendWritesOneDatagramPerMetric(t *testing.T) {
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
	entry := metricEntry{
		LatencyMS:     1,
		ApisixLatency: 1,
		IngressSize:   2,
		EgressSize:    3,
		Status:        200,
	}

	p.Send(entry)

	if got := collectMessages(t, received, 5); !slices.Equal(got, p.metricLines(entry)) {
		t.Fatalf("UDP datagrams = %v, want one datagram per metric %v", got, p.metricLines(entry))
	}
}

func TestSendFallsBackToPerMetricDatagramsAboveDogStatsDLimit(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{})
	p.metadata = Metadata{
		Host:      host,
		Port:      mustAtoi(t, port),
		Namespace: "apisix",
		ConstantTags: []string{
			strings.Repeat("a", 200),
			strings.Repeat("b", 200),
			strings.Repeat("c", 200),
			strings.Repeat("d", 200),
			strings.Repeat("e", 200),
			strings.Repeat("f", 200),
			strings.Repeat("g", 200),
			strings.Repeat("h", 200),
			strings.Repeat("i", 200),
			strings.Repeat("j", 200),
		},
	}
	entry := metricEntry{
		LatencyMS:     1,
		ApisixLatency: 1,
		IngressSize:   2,
		EgressSize:    3,
		Status:        200,
	}

	p.Send(entry)

	if got := collectMessages(t, received, 5); !slices.Equal(got, p.metricLines(entry)) {
		t.Fatalf("UDP datagrams = %v, want one datagram per metric %v", got, p.metricLines(entry))
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

	messages := collectMetricLines(t, received, 5, 5)
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

	messages := collectMetricLines(t, received, 5, 5)
	if !containsLinePart(messages, "response_status:201") {
		t.Fatalf("messages = %v, want response_status tag", messages)
	}
	if !containsLinePart(messages, "path:/orders/1") {
		t.Fatalf("messages = %v, want path tag", messages)
	}
	if !containsPrefix(messages, "apisix.ingress.size:97|ms|#") {
		t.Fatalf("messages = %v, want ingress size", messages)
	}
	if !containsPrefix(messages, "apisix.egress.size:126|ms|#") {
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

	messages := collectMetricLines(t, received, 6, 6)
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

	messages := collectMetricLines(t, received, 5, 5)
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

	messages := collectMetricLines(t, received, 5, 5)
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
	messages := collectMetricLines(t, received, 10, 10)
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
		_ = conn.Close()
	})

	received := make(chan string, count)
	go func() {
		for range count {
			buf := make([]byte, 64*1024)
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

func collectMetricLines(t *testing.T, received <-chan string, datagrams int, count int) []string {
	t.Helper()

	lines := make([]string, 0, count)
	for _, datagram := range collectMessages(t, received, datagrams) {
		lines = append(lines, strings.Split(datagram, "\n")...)
	}
	if len(lines) != count {
		t.Fatalf("metric lines = %d, want %d: %v", len(lines), count, lines)
	}
	return lines
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

type deadlineRecordingConn struct {
	net.Conn
	writeDeadline time.Time
}

func (c *deadlineRecordingConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
}

func TestSendReusesDogStatsDSocketAcrossBatches(t *testing.T) {
	addr, received := startUDPServer(t, 10)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port)})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if err := p.send(context.Background(), metricEntry{LatencyMS: 1, Status: 200}); err != nil {
		t.Fatalf("send #1 error = %v", err)
	}
	if err := p.send(context.Background(), metricEntry{LatencyMS: 2, Status: 201}); err != nil {
		t.Fatalf("send #2 error = %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1 reused socket", got)
	}
	waitDatadogMessages(t, received, 10)
}

func TestSendSetsWriteDeadlinePerSend(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port)})
	var wrapped *deadlineRecordingConn
	p.dialFunc = func() (net.Conn, error) {
		raw, err := net.Dial("udp", addr)
		if err != nil {
			return nil, err
		}
		wrapped = &deadlineRecordingConn{Conn: raw}
		return wrapped, nil
	}

	if err := p.send(context.Background(), metricEntry{LatencyMS: 1, Status: 200}); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	if wrapped == nil || wrapped.writeDeadline.IsZero() {
		t.Fatal("write deadline was not set per send")
	}
	waitDatadogMessages(t, received, 5)
}

func TestSendRedialsAfterSocketFailure(t *testing.T) {
	addr, received := startUDPServer(t, 10)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port)})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if err := p.send(context.Background(), metricEntry{LatencyMS: 1, Status: 200}); err != nil {
		t.Fatalf("send #1 error = %v", err)
	}
	p.connMu.Lock()
	_ = p.conn.Close()
	p.connMu.Unlock()

	if err := p.send(context.Background(), metricEntry{LatencyMS: 2, Status: 201}); err == nil {
		t.Fatal("send #2 error = nil on a closed socket")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count after failed send = %d, want no redial until the next send", got)
	}

	if err := p.send(context.Background(), metricEntry{LatencyMS: 3, Status: 202}); err != nil {
		t.Fatalf("send #3 error = %v, want redial delivery", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count after redial = %d, want 2", got)
	}
	waitDatadogMessages(t, received, 10)
}

func TestStopClosesDogStatsDSocket(t *testing.T) {
	addr, received := startUDPServer(t, 5)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port)})
	p.dialFunc = func() (net.Conn, error) {
		return net.Dial("udp", addr)
	}
	if err := p.send(context.Background(), metricEntry{LatencyMS: 1, Status: 200}); err != nil {
		t.Fatalf("send() error = %v", err)
	}
	waitDatadogMessages(t, received, 5)

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		t.Fatal("Stop() left the DogStatsD socket open")
	}
}

func waitDatadogMessages(t *testing.T, received <-chan string, count int) {
	t.Helper()

	for range count {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for DogStatsD datagram")
		}
	}
}
