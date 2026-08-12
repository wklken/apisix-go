package base

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

func TestBaseLoggerRunLogPhaseUsesBoundedBatchQueue(t *testing.T) {
	started := make(chan struct{})
	processor := logger_batch.NewWithContext(logger_batch.Config{
		Name:              "test",
		BatchMaxSize:      100,
		MaxPendingEntries: 1,
		BufferDuration:    0,
		InactiveTimeout:   0,
		ShutdownTimeout:   1,
	}, func(context.Context, []map[string]any, int) (int, error) {
		<-started
		return 0, nil
	})
	defer func() {
		close(started)
		processor.Stop()
	}()

	plugin := &BaseLoggerPlugin{BatchProcessor: processor, LogFormat: map[string]string{"method": "$request_method"}}
	snapshot := LogSnapshot{Request: apisixlog.RequestLogSnapshot{Method: http.MethodPost}}
	if err := plugin.RunLogPhase(snapshot); err != nil {
		t.Fatalf("first RunLogPhase() error = %v", err)
	}
	if err := plugin.RunLogPhase(snapshot); !errors.Is(err, ErrLogQueueFull) {
		t.Fatalf("second RunLogPhase() error = %v, want ErrLogQueueFull", err)
	}
}

func TestBaseLoggerLogCapturePolicyIncludesExtraFormatBodies(t *testing.T) {
	plugin := &BaseLoggerPlugin{
		SnapshotLogFormatExtra: map[string]any{
			"request":  map[string]any{"body": "$request_body"},
			"response": map[string]any{"body": "$response_body"},
		},
	}

	policy := plugin.LogCapturePolicy()
	if policy.RequestBodyBytes != MAX_REQ_BODY {
		t.Fatalf("request body limit = %d, want %d", policy.RequestBodyBytes, MAX_REQ_BODY)
	}
	if policy.ResponseBodyBytes != MAX_RESP_BODY {
		t.Fatalf("response body limit = %d, want %d", policy.ResponseBodyBytes, MAX_RESP_BODY)
	}
}

func TestLogCapturePolicyForFormatsIncludesNestedAndStringBodies(t *testing.T) {
	policy := LogCapturePolicyForFormats(
		17,
		23,
		map[string]any{"nested": map[string]any{"body": "$request_body"}},
		map[string]string{"response": "$response_body"},
	)
	if policy.RequestBodyBytes != 17 || policy.ResponseBodyBytes != 23 {
		t.Fatalf("policy = %#v, want request=17 response=23", policy)
	}
}

func TestSnapshotExpressionMatchesStatus(t *testing.T) {
	snapshot := LogSnapshot{Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated}}
	if !SnapshotExpressionMatches(snapshot, [][]any{{"$status", "==", 201}}) {
		t.Fatal("status expression did not match")
	}
}

func TestSnapshotExpressionMatchesPreservesLegacyNestedOperators(t *testing.T) {
	snapshot := LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Header: http.Header{"X-Environment": []string{"production"}},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated},
	}
	expressions := [][]any{
		{"$http_x_environment", "==", "production"},
		{"AND"},
		{"$status", "==", http.StatusCreated},
	}
	if !SnapshotExpressionMatches(snapshot, expressions) {
		t.Fatal("snapshot expression rejected the legacy nested operator shape")
	}
}

func TestSnapshotResponseBodyStopsDecodingAtBindingLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("expanded-sensitive-body"), 1<<15)
	tests := []struct {
		name     string
		encoding string
		compress func(*testing.T, []byte) []byte
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			compress: func(t *testing.T, body []byte) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(body); err != nil {
					t.Fatalf("gzip write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				return compressed.Bytes()[:compressed.Len()-4]
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			compress: func(t *testing.T, body []byte) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := brotli.NewWriter(&compressed)
				if _, err := writer.Write(body[:128]); err != nil {
					t.Fatalf("brotli write: %v", err)
				}
				if err := writer.Flush(); err != nil {
					t.Fatalf("brotli flush: %v", err)
				}
				if _, err := writer.Write(body[128:]); err != nil {
					t.Fatalf("brotli write remainder: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("brotli close: %v", err)
				}
				return compressed.Bytes()[:compressed.Len()-1]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := LogSnapshot{}
			snapshot.Response.Header = http.Header{"Content-Encoding": []string{test.encoding}}
			snapshot.Response.Body = test.compress(t, payload)
			if got, want := SnapshotResponseBody(snapshot, 32), string(payload[:32]); got != want {
				t.Fatalf("SnapshotResponseBody() = %q, want decoded prefix %q", got, want)
			}
		})
	}
}

func TestBasePluginExposesStablePluginContract(t *testing.T) {
	plugin := &BasePlugin{Name: "logger", Priority: 10, Schema: "schema", MetadataSchema: "metadata"}
	plugin.SetPriority(20)
	if plugin.GetName() != "logger" || plugin.GetPriority() != 20 || plugin.GetSchema() != "schema" ||
		plugin.GetMetadataSchema() != "metadata" {
		t.Fatalf("base plugin contract = %#v", plugin)
	}
}

func TestBaseLoggerRunLogPhaseBuildsDetachedNestedPayload(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	processor := logger_batch.NewWithContext(logger_batch.Config{
		Name:              "detached-payload",
		BatchMaxSize:      1,
		MaxPendingEntries: 4,
		BufferDuration:    time.Second,
		InactiveTimeout:   time.Second,
		ShutdownTimeout:   time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return len(entries), nil
	})
	defer processor.Stop()

	plugin := &BaseLoggerPlugin{BatchProcessor: processor}
	plugin.SetLogCapturePolicy(true, true, 4, 5, [][]any{{"$status", "==", 201}}, nil)
	plugin.SetSnapshotLogFormat(
		map[string]any{"request": map[string]any{"method": "$request_method"}},
		map[string]any{"source": "$response_source"},
	)
	snapshot := LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Method: http.MethodPost, Body: []byte("request")},
		Response: apisixlog.ResponseLogSnapshot{
			Header: http.Header{"Content-Encoding": {"identity"}}, Body: []byte("response"),
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated},
		Source:  apisixctx.ResponseSourceUpstream,
	}
	if err := plugin.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		request := fields["request"].(map[string]any)
		response := fields["response"].(map[string]any)
		if request["method"] != http.MethodPost || request["body"] != "requ" || response["body"] != "respo" ||
			fields["source"] != "upstream" {
			t.Fatalf("detached fields = %#v", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("detached payload was not delivered")
	}
	if err := plugin.Fire(map[string]any{"compatibility": true}); err != nil {
		t.Fatalf("Fire() with processor error = %v", err)
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("compatibility Fire payload was not delivered")
	}
	plugin.StopWithCleanup(nil)
}

func TestBaseLoggerCapturePolicyAndSnapshotHelpersCoverBoundaries(t *testing.T) {
	plugin := &BaseLoggerPlugin{
		IncludeRequestBody:  true,
		IncludeResponseBody: true,
		RequestBodyBytes:    9,
		ResponseBodyBytes:   11,
		LogFormat:           map[string]string{"request": "$request_body", "response": "$resp_body"},
	}
	if got := plugin.LogCapturePolicy(); got.RequestBodyBytes != 9 || got.ResponseBodyBytes != 11 {
		t.Fatalf("explicit capture policy = %#v", got)
	}
	if got := LogCapturePolicyForFormats(0, 0,
		map[string]any{"request": "$request_body"},
		map[string]string{"response": "$resp_body"},
	); got.RequestBodyBytes != MAX_REQ_BODY || got.ResponseBodyBytes != MAX_RESP_BODY {
		t.Fatalf("defaulted capture policy = %#v", got)
	}
	if formatContainsExpression([]any{map[string]string{"literal": "value"}}, "$request_body") {
		t.Fatal("literal format unexpectedly selected request-body capture")
	}

	snapshot := LogSnapshot{
		Request:  apisixlog.RequestLogSnapshot{Method: http.MethodGet, Body: []byte("request")},
		Response: apisixlog.ResponseLogSnapshot{Body: []byte("response")},
	}
	if got := SnapshotValue(snapshot, "literal"); got != "literal" {
		t.Fatalf("literal SnapshotValue() = %#v", got)
	}
	if got := SnapshotValue(snapshot, "$request_method"); got != http.MethodGet {
		t.Fatalf("variable SnapshotValue() = %#v", got)
	}
	if SnapshotRequestBody(snapshot, 0) != "" || SnapshotRequestBody(snapshot, 3) != "req" {
		t.Fatal("SnapshotRequestBody() boundary semantics changed")
	}
	if SnapshotResponseBody(snapshot, 0) != "" || SnapshotResponseBody(LogSnapshot{}, 3) != "" ||
		SnapshotResponseBody(snapshot, 4) != "resp" || SnapshotResponseBody(snapshot, 20) != "response" {
		t.Fatal("SnapshotResponseBody() boundary semantics changed")
	}
	snapshot.Response.Header = http.Header{"Content-Encoding": {"gzip"}}
	snapshot.Response.Body = []byte("not-gzip")
	if got := SnapshotResponseBody(snapshot, 3); got != "not" {
		t.Fatalf("invalid gzip fallback = %q, want %q", got, "not")
	}
}

func TestSnapshotExpressionMatchesRebuildsDetachedRequest(t *testing.T) {
	snapshot := LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method:      http.MethodGet,
			URI:         "/orders",
			URL:         ":invalid",
			Host:        "gateway.test",
			RemoteAddr:  "192.0.2.1:1234",
			Scheme:      "https",
			Header:      http.Header{},
			Query:       map[string][]string{"q": {"one"}},
			APISIXVars:  map[string]any{"$route_id": "route-1"},
			RequestVars: map[string]any{"$request_id": "request-1"},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusAccepted},
	}
	expressions := [][]any{
		{"$arg_q", "==", "one"},
		{"AND"},
		{"$scheme", "==", "https"},
		{"AND"},
		{"$route_id", "==", "route-1"},
	}
	if !SnapshotExpressionMatches(snapshot, expressions) {
		t.Fatal("rebuilt detached request did not preserve query, scheme, or variables")
	}
}

func TestBaseLoggerConfigurationDefaultsAndCompatibilityHandler(t *testing.T) {
	plugin := &BaseLoggerPlugin{}
	plugin.SetRouteContext("route-1", "127.0.0.1:9080")
	plugin.InitLogger(func(map[string]any) {})
	if plugin.RouteID != "route-1" || plugin.ServerAddr != "127.0.0.1:9080" ||
		plugin.FireChan == nil || !plugin.AsyncBlock || plugin.SendFunc == nil {
		t.Fatalf("logger initialization = %#v", plugin)
	}

	nextCalled := false
	handler := plugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
	if !nextCalled {
		t.Fatal("compatibility Handler did not call downstream")
	}
	select {
	case <-plugin.FireChan:
	default:
		t.Fatal("compatibility Handler did not enqueue a log entry")
	}
	dropper := &BaseLoggerPlugin{FireChan: make(chan map[string]any, 1)}
	dropper.FireChan <- map[string]any{"first": true}
	if err := dropper.Fire(map[string]any{"dropped": true}); err != nil {
		t.Fatalf("non-blocking compatibility Fire() error = %v", err)
	}

	defaults := BatchDefaults{MaxConcurrentDeliveries: 100}
	ApplyBatchDefaults(&defaults)
	if defaults.BatchMaxSize <= 0 || defaults.RetryDelaySec <= 0 || defaults.BufferDurationSec <= 0 ||
		defaults.InactiveTimeoutSec <= 0 || defaults.MaxPendingEntries <= 0 ||
		defaults.MaxConcurrentDeliveries != 8 || defaults.DeliveryTimeoutSec <= 0 || defaults.ShutdownTimeoutSec <= 0 {
		t.Fatalf("batch defaults = %#v", defaults)
	}

	cleanupCalls := 0
	stoppable := &BaseLoggerPlugin{}
	stoppable.StopWithCleanup(func() { cleanupCalls++ })
	stoppable.StopWithCleanup(func() { cleanupCalls++ })
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
	stoppable.Stop()

	if err := EnqueueLog(nil, map[string]any{}); !errors.Is(err, ErrLogQueueUnavailable) {
		t.Fatalf("EnqueueLog(nil) error = %v", err)
	}
}
