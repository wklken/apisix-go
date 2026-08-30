package skywalking_logger

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPostInitWarnsOnlyForInsecureEndpointAddr(t *testing.T) {
	for _, test := range []struct {
		name     string
		scheme   string
		wantWarn bool
	}{
		{name: "http", scheme: "http", wantWarn: true},
		{name: "https", scheme: "https"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings []string
			stop := logger.ReplaceObserver("skywalking-logger-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "skywalking-logger endpoint_addr") {
					warnings = append(warnings, entry.Message)
				}
			})
			defer stop()

			p := newTestPlugin(t, Config{EndpointAddr: test.scheme + "://127.0.0.1:12800"})
			t.Cleanup(p.Stop)

			if test.wantWarn {
				if len(warnings) != 1 ||
					warnings[0] != "Using skywalking-logger endpoint_addr with no TLS is a security risk" {
					t.Fatalf("warnings = %#v, want exact insecure endpoint warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %#v, want none for TLS endpoint", warnings)
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

	return p
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t), Metadata: mustMetadataView(t, metadata)})
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

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestMetadataSchemaAcceptsLogFormatAndPendingLimit(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"log_format":          map[string]any{"generation": "$route_id"},
		"max_pending_entries": 1,
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, metadata := range []map[string]any{
		{"log_format": "$route_id"},
		{"max_pending_entries": 0},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("invalid metadata accepted: %#v", metadata)
		}
	}
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	first := newTestPluginWithMetadata(t, Config{
		EndpointAddr: "http://127.0.0.1:12800",
	}, map[string]any{
		"log_format":          map[string]any{"generation": "n"},
		"max_pending_entries": 11,
	})
	second := newTestPluginWithMetadata(t, Config{
		EndpointAddr: "http://127.0.0.1:12801",
	}, map[string]any{
		"log_format":          map[string]any{"generation": "n-plus-one"},
		"max_pending_entries": 12,
	})

	if first.LogFormat["generation"] != "n" || first.config.MaxPendingEntries != 11 {
		t.Fatalf("generation N metadata = %#v/%d", first.LogFormat, first.config.MaxPendingEntries)
	}
	if second.LogFormat["generation"] != "n-plus-one" || second.config.MaxPendingEntries != 12 {
		t.Fatalf("generation N+1 metadata = %#v/%d", second.LogFormat, second.config.MaxPendingEntries)
	}
}

func TestMetadataDecodeFailsBeforeSkyWalkingClientAndProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{EndpointAddr: "http://127.0.0.1:12800"}}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t), Metadata: mustMetadataView(t, map[string]any{
		"max_pending_entries": "invalid",
	})})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"decode failure acquired SkyWalking resources: client=%v release=%v processor=%v",
			p.client,
			p.clientRelease != nil,
			p.BatchProcessor,
		)
	}
}

func TestPostInitSetsSkyWalkingDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "http://127.0.0.1:12800",
	})

	if p.config.ServiceName != "APISIX" {
		t.Fatalf("service_name = %q, want APISIX", p.config.ServiceName)
	}
	if p.config.ServiceInstanceName != "APISIX Instance Name" {
		t.Fatalf("service_instance_name = %q, want APISIX Instance Name", p.config.ServiceInstanceName)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
}

func TestEndpointURLAppendsLogsPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "http://127.0.0.1:12800/",
	})

	if got := p.endpointURL(); got != "http://127.0.0.1:12800/v3/logs" {
		t.Fatalf("endpointURL() = %q, want /v3/logs appended once", got)
	}
}

func TestBuildEntryUsesSkyWalkingLogShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr:        "http://127.0.0.1:12800",
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		LogFormat:           map[string]string{"path": "$uri"},
		IncludeReqBody:      true,
		MaxReqBodyBytes:     128,
		MaxRespBodyBytes:    256,
		IncludeRespBody:     true,
	})

	entry, err := p.buildEntry(map[string]any{
		"path":                     "/orders",
		internalSkyWalkingEndpoint: "/orders",
	})
	if err != nil {
		t.Fatalf("buildEntry() error = %v", err)
	}

	if entry.Service != "gateway" {
		t.Fatalf("service = %q, want gateway", entry.Service)
	}
	if entry.ServiceInstance != "instance-a" {
		t.Fatalf("serviceInstance = %q, want instance-a", entry.ServiceInstance)
	}
	if entry.Endpoint != "/orders" {
		t.Fatalf("endpoint = %q, want /orders", entry.Endpoint)
	}
	if entry.Body.JSON.JSON == "" {
		t.Fatal("body.json.json is empty")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(entry.Body.JSON.JSON), &payload); err != nil {
		t.Fatalf("decode SkyWalking json body: %v", err)
	}
	if payload["path"] != "/orders" {
		t.Fatalf("payload path = %v, want /orders", payload["path"])
	}
	if _, ok := payload[internalSkyWalkingEndpoint]; ok {
		t.Fatalf("payload includes internal endpoint marker: %#v", payload)
	}
}

func TestHandlerBodyCaptureMatrix(t *testing.T) {
	tests := []struct {
		name, requestBody, responseBody, header string
		requestExpr, responseExpr               [][]any
		wantBodies                              bool
	}{
		{name: "unconditional", requestBody: `{"order":1}`, responseBody: `{"ok":true}`, wantBodies: true},
		{
			name:         "expressions match",
			requestBody:  `{"order":2}`,
			responseBody: `{"created":true}`,
			header:       "yes",
			requestExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
			responseExpr: [][]any{{"status", "==", "201"}},
			wantBodies:   true,
		},
		{
			name:         "expressions miss",
			requestBody:  `{"order":3}`,
			responseBody: `{"created":false}`,
			header:       "no",
			requestExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
			responseExpr: [][]any{{"status", "==", "500"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := make(chan []skyWalkingEntry, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body []skyWalkingEntry
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request body: %v", err)
					return
				}
				entries <- body
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				EndpointAddr:        server.URL,
				ServiceName:         "gateway",
				ServiceInstanceName: "instance-a",
				Timeout:             1,
				BatchMaxSize:        1,
				IncludeReqBody:      true,
				IncludeReqBodyExpr:  test.requestExpr,
				IncludeRespBody:     true,
				IncludeRespBodyExpr: test.responseExpr,
				MaxReqBodyBytes:     32,
				MaxRespBodyBytes:    32,
			})
			req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(test.requestBody))
			if test.header != "" {
				req.Header.Set("X-Log-Body", test.header)
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read upstream request body: %v", err)
				}
				if string(body) != test.requestBody {
					t.Fatalf("upstream body = %q, want %q", body, test.requestBody)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(test.responseBody))
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated || rr.Body.String() != test.responseBody {
				t.Fatalf(
					"response = (%d, %q), want (%d, %q)",
					rr.Code,
					rr.Body.String(),
					http.StatusCreated,
					test.responseBody,
				)
			}

			select {
			case body := <-entries:
				if len(body) != 1 {
					t.Fatalf("entries = %d, want 1", len(body))
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(body[0].Body.JSON.JSON), &payload); err != nil {
					t.Fatalf("decode SkyWalking payload: %v", err)
				}
				if !test.wantBodies {
					if _, ok := payload["request"]; ok {
						t.Fatalf("payload request = %#v, want omitted", payload["request"])
					}
					if _, ok := payload["response"]; ok {
						t.Fatalf("payload response = %#v, want omitted", payload["response"])
					}
					return
				}
				request := payload["request"].(map[string]any)
				response := payload["response"].(map[string]any)
				if request["body"] != test.requestBody || response["body"] != test.responseBody {
					t.Fatalf(
						"logged bodies = (%#v, %#v), want (%q, %q)",
						request["body"],
						response["body"],
						test.requestBody,
						test.responseBody,
					)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for SkyWalking handler delivery")
			}
		})
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":          "http://127.0.0.1:12800",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":       "http://127.0.0.1:12800",
		"batch_max_size":      2,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     60,
		"inactive_timeout":    5,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch and max pending fields: %v", err)
	}
}

func TestParseTraceContextFromSW8(t *testing.T) {
	traceID := base64.RawURLEncoding.EncodeToString([]byte("trace-id"))
	segmentID := base64.RawURLEncoding.EncodeToString([]byte("segment-id"))
	parentService := base64.RawURLEncoding.EncodeToString([]byte("parent-service"))
	parentInstance := base64.RawURLEncoding.EncodeToString([]byte("parent-instance"))
	parentEndpoint := base64.RawURLEncoding.EncodeToString([]byte("parent-endpoint"))
	trace, err := parseTraceContext(
		"1-" + traceID + "-" + segmentID + "-7-" + parentService + "-" + parentInstance + "-" + parentEndpoint + "-ipport",
	)
	if err != nil {
		t.Fatalf("parseTraceContext() error = %v", err)
	}
	if trace.TraceID != "trace-id" {
		t.Fatalf("traceId = %q, want trace-id", trace.TraceID)
	}
	if trace.TraceSegmentID != "segment-id" {
		t.Fatalf("traceSegmentId = %q, want segment-id", trace.TraceSegmentID)
	}
	if trace.SpanID != 7 {
		t.Fatalf("spanId = %d, want 7", trace.SpanID)
	}
}

func TestParseTraceContextIdentifiesMalformedSevenPartHeader(t *testing.T) {
	header := "1-YWU3MDk3NjktNmUyMC00YzY4LTk3MzMtMTBmNDU1MjE2Y2M1-" +
		"YWU3MDk3NjktNmUyMC00YzY4LTk3MzMtMTBmNDU1MjE2Y2M1-1-QVBJU0lY-" +
		"QVBJU0lYIEluc3RhbmNlIE5hbWU=-L2dldA=="
	trace, err := parseTraceContext(header)
	if trace != nil {
		t.Fatalf("trace = %#v, want nil", trace)
	}
	if err == nil || !strings.Contains(err.Error(), "got 7 parts, want 8") {
		t.Fatalf("parseTraceContext() error = %v, want identifying part-count diagnostic", err)
	}
}

func TestSendPostsSkyWalkingEntries(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		Timeout:             1,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/v3/logs" {
			t.Fatalf("path = %q, want /v3/logs", req.URL.Path)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking request")
	}

	select {
	case body := <-bodies:
		if len(body) != 1 {
			t.Fatalf("entries = %d, want 1", len(body))
		}
		if body[0]["service"] != "gateway" {
			t.Fatalf("service = %v, want gateway", body[0]["service"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking body")
	}
}

func TestHandlerBatchesSkyWalkingEntries(t *testing.T) {
	bodies := make(chan []skyWalkingEntry, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []skyWalkingEntry
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		Timeout:             1,
		BatchMaxSize:        2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))

	select {
	case body := <-bodies:
		if len(body) != 2 {
			t.Fatalf("entries = %d, want 2", len(body))
		}
		if body[0].Endpoint != "/first" || body[1].Endpoint != "/second" {
			t.Fatalf("endpoints = %q, %q; want /first, /second", body[0].Endpoint, body[1].Endpoint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched SkyWalking body")
	}
}

func TestHandlerCustomFormatResolvesAPISIXValuesAndRouteIdentity(t *testing.T) {
	entries := make(chan []skyWalkingEntry, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []skyWalkingEntry
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		entries <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "APISIX",
		ServiceInstanceName: "instance-a",
		Timeout:             1,
		BatchMaxSize:        1,
		LogFormat: map[string]string{
			"host":       "$host",
			"@timestamp": "$time_iso8601",
			"client_ip":  "$remote_addr",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/opentracing", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.10:43123"
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":   "route-1",
		"$service_id": "service-1",
	})
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	select {
	case body := <-entries:
		if len(body) != 1 {
			t.Fatalf("entries = %d, want 1", len(body))
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body[0].Body.JSON.JSON), &payload); err != nil {
			t.Fatalf("decode SkyWalking payload: %v", err)
		}
		if payload["host"] != "gateway.example" {
			t.Fatalf("host = %#v, want gateway.example", payload["host"])
		}
		if payload["client_ip"] != "192.0.2.10" {
			t.Fatalf("client_ip = %#v, want 192.0.2.10", payload["client_ip"])
		}
		if timestamp, ok := payload["@timestamp"].(string); !ok || timestamp == "" {
			t.Fatalf("@timestamp = %#v, want non-empty string", payload["@timestamp"])
		}
		if payload["route_id"] != "route-1" || payload["service_id"] != "service-1" {
			t.Fatalf(
				"route/service identity = %#v/%#v, want route-1/service-1",
				payload["route_id"],
				payload["service_id"],
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking custom-format delivery")
	}
}

func TestBuildEntrySurfacesMarshalError(t *testing.T) {
	p := newTestPlugin(t, Config{ServiceName: "gateway"})

	_, err := p.buildEntry(map[string]any{"bad": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("buildEntry() error = %v, want marshal failure surfaced", err)
	}
}
