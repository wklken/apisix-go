package route

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
)

type metadataLogContractPlugin struct {
	metadataResponseContractPlugin
	requestCalls   int
	logCalls       int
	finalizerCalls int
}

type metadataPhaseConfig struct {
	descriptor base.BindingPhaseDescriptor
}

func (c metadataPhaseConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return c.descriptor, nil
}

type metadataLogResponseContractPlugin struct {
	metadataLogContractPlugin
	config metadataPhaseConfig
}

type metadataSanitizerContractPlugin struct {
	metadataResponseContractPlugin
	calls    int
	sanitize func(*base.LogSnapshot)
}

func (p *metadataSanitizerContractPlugin) LogCapturePolicy() base.LogCapturePolicy {
	return base.LogCapturePolicy{RequestBodyBytes: 5}
}

func (p *metadataSanitizerContractPlugin) SanitizeLogSnapshot(snapshot *base.LogSnapshot) error {
	p.calls++
	if p.sanitize != nil {
		p.sanitize(snapshot)
		return nil
	}
	snapshot.Request.Header.Set("Authorization", "[REDACTED]")
	return nil
}

func (p *metadataLogResponseContractPlugin) Config() any { return p.config }

func TestRoutePipelineInstallsStaticLogExecutorBeforeTerminal(t *testing.T) {
	target := &metadataLogContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "http-logger",
	}}
	binding := pluginpkg.BindPlugin(
		"http-logger",
		target,
		pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "log-route"},
	)
	pipeline, err := newRequestPipelineWithLog([]pluginpkg.Binding{binding}, nil)
	if err != nil {
		t.Fatalf("newRequestPipelineWithLog() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Unix(1, 0),
	)
	pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %v", failures)
	}
	if target.logCalls != 1 {
		t.Fatalf("log calls = %d, want 1", target.logCalls)
	}
}

func TestPluginPhaseClosureBuildsAuthCORSResponseRewriteAndLogger(t *testing.T) {
	ensureRouteStore(t)
	deleteEvent := store.NewEvent()
	deleteEvent.Type = store.EventTypeDelete
	deleteEvent.Key = []byte("/apisix/plugin_metadata/cors")
	routeStoreEvents <- deleteEvent
	if err := routeStore.Sync(); err != nil {
		t.Fatalf("clear CORS metadata: %v", err)
	}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	logged := make(chan map[string]any, 1)
	logSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read logger payload: %v", readErr)
		} else {
			var payload map[string]any
			if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
				t.Errorf("decode logger payload %q: %v", body, decodeErr)
			} else {
				logged <- payload
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logSink.Close)

	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(resource.Route{
		ID: "phase-closure",
		Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{},
			"key-auth":   map[string]any{},
			"cors": map[string]any{
				"allow_origins": "*",
			},
			"response-rewrite": map[string]any{
				"headers": map[string]any{"set": map[string]any{"X-Phase": "explicit"}},
			},
			"http-logger": map[string]any{
				"uri":            logSink.URL,
				"batch_max_size": 1,
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	})
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/phase", nil)
	request.Header.Set("Origin", "https://client.test")
	request.Header.Set("X-Request-Id", "phase-request-1")
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	request = apisixctx.WithApisixVars(request, map[string]string{"$closure_marker": "live"})
	request = apisixctx.WithRequestVars(request)
	finalizerCalls := 0
	var sourceDuringFinalize apisixctx.ResponseSource
	var outcomeDuringFinalize apisixctx.ResponseOutcome
	var markerDuringFinalize any
	if !lifecycle.AddFinalizer("phase-closure-observer", func() error {
		finalizerCalls++
		sourceDuringFinalize = lifecycle.ResponseSource()
		outcomeDuringFinalize = lifecycle.Outcome()
		markerDuringFinalize = apisixctx.GetApisixVar(request, "$closure_marker")
		return nil
	}) {
		t.Fatal("failed to register phase closure finalizer")
	}
	handler.ServeHTTP(response, request)
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code,
		Bytes: int64(response.Body.Len()), Committed: true,
	}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("finalize failures = %#v", failures)
	}
	apisixctx.RecycleVars(request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("auth rejection status = %d, want 401", response.Code)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want zero after auth rejection", upstreamCalls)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "*" ||
		response.Header().Get("X-Phase") != "explicit" {
		t.Fatalf("response phase headers = %#v", response.Header())
	}
	if finalizerCalls != 1 || sourceDuringFinalize != apisixctx.ResponseSourceEarlyStop ||
		outcomeDuringFinalize.Status != http.StatusUnauthorized || markerDuringFinalize != "live" {
		t.Fatalf(
			"finalizer state = calls:%d source:%q outcome:%#v marker:%#v",
			finalizerCalls,
			sourceDuringFinalize,
			outcomeDuringFinalize,
			markerDuringFinalize,
		)
	}
	if marker := apisixctx.GetApisixVar(request, "$closure_marker"); marker != "" {
		t.Fatalf("marker after recycle = %#v, want empty", marker)
	}
	select {
	case payload := <-logged:
		responseFields, ok := payload["response"].(map[string]any)
		if !ok || responseFields["status"] != float64(http.StatusUnauthorized) {
			t.Fatalf("logger payload = %#v", payload)
		}
		if payload["request_id"] != "phase-request-1" || payload["response_source"] != "early_stop" ||
			payload["outcome"] != "completed" {
			t.Fatalf("logger correlation fields = %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("auth rejection did not execute detached logger exactly once")
	}
	select {
	case extra := <-logged:
		t.Fatalf("logger executed more than once: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPluginPhaseClosureServerlessLogNowCoexistsWithStreaming(t *testing.T) {
	ensureRouteStore(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(resource.Route{
		ID: "serverless-log-streaming",
		Plugins: map[string]resource.PluginConfig{
			"proxy-buffering": map[string]any{"disable_proxy_buffering": true},
			"serverless-pre-function": map[string]any{
				"phase":     "log",
				"functions": []any{`return function() error("serverless log invoked") end`},
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	})
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/stream", nil),
		time.Now(),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code, Committed: true,
	}, time.Now())
	failures := lifecycle.Finalize()
	apisixctx.RecycleVars(request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if len(failures) != 1 || failures[0].Err == nil ||
		!strings.Contains(failures[0].Err.Error(), "serverless log invoked") {
		t.Fatalf("serverless log failures = %#v, want exactly one invocation", failures)
	}
}

func TestDataMaskRoutePreservesUpstreamAndSanitizesDetachedLogger(t *testing.T) {
	ensureRouteStore(t)
	type upstreamObservation struct {
		requestURI    string
		authorization string
		requestID     string
		body          string
	}
	upstreamSeen := make(chan upstreamObservation, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		upstreamSeen <- upstreamObservation{
			requestURI:    request.RequestURI,
			authorization: request.Header.Get("Authorization"),
			requestID:     request.Header.Get("X-Request-Id"),
			body:          string(body),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	logged := make(chan map[string]any, 1)
	logSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read logger payload: %v", readErr)
		} else {
			var payload map[string]any
			if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
				t.Errorf("decode logger payload %q: %v", body, decodeErr)
			} else {
				logged <- payload
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logSink.Close)

	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(resource.Route{
		ID:  "data-mask-detached-log",
		Uri: "/mask",
		Plugins: map[string]resource.PluginConfig{
			"data-mask": map[string]any{
				"request": []any{
					map[string]any{"type": "query", "name": "token", "action": "replace", "value": "***"},
					map[string]any{"type": "header", "name": "Authorization", "action": "remove"},
					map[string]any{
						"type": "header", "name": "X-Request-Id", "action": "replace", "value": "masked-id",
					},
					map[string]any{
						"type": "body", "body_format": "json", "name": "$.token",
						"action": "replace", "value": "***",
					},
				},
			},
			"http-logger": map[string]any{
				"uri":              logSink.URL,
				"batch_max_size":   1,
				"include_req_body": true,
			},
		},
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: upstreamURL.Scheme,
			Nodes:  []resource.Node{{Host: upstreamURL.Hostname(), Port: port, Weight: 1}},
		},
	})
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
	const originalURI = "/mask?token=one&keep=%2f&token=two"
	const originalBody = " {\n \"token\":\"secret\", \"amount\":1.00\n} "
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test"+originalURI, strings.NewReader(originalBody))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-Id", "raw-request-id")
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	request = apisixctx.WithRequestVars(request)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code, Committed: true,
	}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	apisixctx.RecycleVars(request)

	select {
	case seen := <-upstreamSeen:
		if seen.requestURI != originalURI || seen.authorization != "Bearer secret" ||
			seen.requestID != "raw-request-id" || seen.body != originalBody {
			t.Fatalf("upstream request changed: %#v", seen)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request was not observed")
	}
	select {
	case payload := <-logged:
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatalf("marshal logger payload: %v", marshalErr)
		}
		text := string(encoded)
		for _, secret := range []string{
			"Bearer secret", `"token":"secret"`, "token=one", "token=two", "raw-request-id",
		} {
			if strings.Contains(text, secret) {
				t.Fatalf("logger payload leaked %q: %s", secret, text)
			}
		}
		if payload["request_id"] != "masked-id" || !strings.Contains(text, `\"token\":\"***\"`) ||
			!strings.Contains(text, "token=%2A%2A%2A") {
			t.Fatalf("logger payload is not fully sanitized: %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("detached logger did not receive sanitized payload")
	}
}

func (p *metadataLogContractPlugin) RunRequestPhase(
	_ http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	p.requestCalls++
	return base.ContinueRequest(r)
}

func (p *metadataLogContractPlugin) RunLogPhase(base.LogSnapshot) error {
	p.logCalls++
	return nil
}

func (p *metadataLogContractPlugin) RunSnapshotFinalizer(base.LogSnapshot) error {
	p.finalizerCalls++
	return nil
}

func (p *metadataLogContractPlugin) LogCapturePolicy() base.LogCapturePolicy {
	return base.LogCapturePolicy{RequestBodyBytes: 7, ResponseBodyBytes: 9}
}

func TestMetadataRequestContextOwnsOnlyRequestPhase(t *testing.T) {
	target := &metadataLogContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "request-context",
	}}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrapper, err := newMetadataPlugin("request-context", target, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin() error = %v", err)
	}
	requestPhase, ok := wrapper.(base.RequestPhasePlugin)
	if !ok {
		t.Fatalf("metadata request-context lost request phase: %T", wrapper)
	}
	if _, ok := wrapper.(base.LogPhasePlugin); ok {
		t.Fatalf("metadata request-context gained undeclared log phase: %T", wrapper)
	}
	if _, ok := wrapper.(base.SnapshotFinalizerPlugin); ok {
		t.Fatalf("metadata request-context retained removed snapshot finalizer: %T", wrapper)
	}
	if _, ok := wrapper.(base.LogCapturePolicyPlugin); ok {
		t.Fatalf("metadata request-context retained log capture policy: %T", wrapper)
	}

	request := httptest.NewRequest(http.MethodGet, "/?enabled=yes", nil)
	if result := requestPhase.RunRequestPhase(httptest.NewRecorder(), request); result.Request == nil {
		t.Fatal("RunRequestPhase() returned nil request")
	}
	request = httptest.NewRequest(http.MethodGet, "/?enabled=no", nil)
	_ = requestPhase.RunRequestPhase(httptest.NewRecorder(), request)
	if target.requestCalls != 1 || target.logCalls != 0 || target.finalizerCalls != 0 {
		t.Fatalf(
			"callback counts = request:%d log:%d finalizer:%d",
			target.requestCalls,
			target.logCalls,
			target.finalizerCalls,
		)
	}
}

func TestMetadataSnapshotFilterPreservesRequestValueSemantics(t *testing.T) {
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		URI:    "/orders?q=one",
		Header: http.Header{"X-Role": {"admin", "operator"}},
	}}
	for name, condition := range map[string][]any{
		"uri":             {[]any{"uri", "==", "/orders"}},
		"repeated_header": {[]any{"http_x_role", "==", "admin,operator"}},
	} {
		filter, err := pluginexpr.Compile(condition)
		if err != nil {
			t.Fatalf("compile %s filter: %v", name, err)
		}
		if !metadataSnapshotFilterMatches(filter, snapshot) {
			t.Fatalf("detached filter changed live %s semantics", name)
		}
	}
}

func TestMetadataLogSanitizerPreservesExactOwnerAndDetachedFilter(t *testing.T) {
	target := &metadataSanitizerContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "data-mask",
	}}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrapper, err := newMetadataPlugin("data-mask", target, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(data-mask) error = %v", err)
	}
	sanitizer, ok := wrapper.(base.LogSnapshotSanitizerPlugin)
	if !ok {
		t.Fatalf("metadata data-mask lost sanitizer callback: %T", wrapper)
	}
	selector, ok := wrapper.(base.LogSnapshotSanitizerSelectorPlugin)
	if !ok {
		t.Fatalf("metadata data-mask lost pre-sanitized selector: %T", wrapper)
	}
	if _, ok := wrapper.(base.LogPhasePlugin); ok {
		t.Fatalf("metadata data-mask gained log callback: %T", wrapper)
	}
	if _, ok := wrapper.(base.SnapshotFinalizerPlugin); ok {
		t.Fatalf("metadata data-mask gained finalizer callback: %T", wrapper)
	}
	policy, ok := wrapper.(base.LogCapturePolicyPlugin)
	if !ok || policy.LogCapturePolicy().RequestBodyBytes != 5 {
		t.Fatalf("metadata data-mask policy = %#v/%v", policy, ok)
	}
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		Query:  url.Values{"enabled": {"no"}},
		Header: http.Header{"Authorization": {"Bearer secret"}},
	}}
	if selector.ShouldSanitizeLogSnapshot(snapshot) {
		t.Fatal("disabled sanitizer selector = true")
	}
	if target.calls != 0 || snapshot.Request.Header.Get("Authorization") != "Bearer secret" {
		t.Fatalf("disabled sanitizer changed snapshot: calls=%d header=%q", target.calls, snapshot.Request.Header)
	}
	snapshot.Request.Query.Set("enabled", "yes")
	if !selector.ShouldSanitizeLogSnapshot(snapshot) {
		t.Fatal("enabled sanitizer selector = false")
	}
	if err := sanitizer.SanitizeLogSnapshot(&snapshot); err != nil {
		t.Fatalf("enabled SanitizeLogSnapshot() error = %v", err)
	}
	if target.calls != 1 || snapshot.Request.Header.Get("Authorization") != "[REDACTED]" {
		t.Fatalf("enabled sanitizer state: calls=%d header=%q", target.calls, snapshot.Request.Header)
	}
}

func TestMetadataLogSanitizersEvaluateFiltersAgainstSamePreSanitizedSnapshot(t *testing.T) {
	global := &metadataSanitizerContractPlugin{
		metadataResponseContractPlugin: metadataResponseContractPlugin{name: "data-mask"},
		sanitize: func(snapshot *base.LogSnapshot) {
			snapshot.Request.Query.Set("enabled", "masked")
		},
	}
	route := &metadataSanitizerContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "data-mask",
	}}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrappedRoute, err := newMetadataPlugin("data-mask", route, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(data-mask) error = %v", err)
	}
	logger := &metadataLogContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "http-logger",
	}}
	bindings := []pluginpkg.Binding{
		pluginpkg.BindPlugin("data-mask", global, pluginpkg.ScopeGlobal,
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceGlobalRule, ID: "global-mask"}),
		pluginpkg.BindPlugin("data-mask", wrappedRoute, pluginpkg.ScopeRoute,
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "route-mask"}),
		pluginpkg.BindPlugin("http-logger", logger, pluginpkg.ScopeRoute,
			pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "route-mask"}),
	}
	executor, err := pluginpkg.NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/?enabled=yes", nil),
		time.Unix(1, 0),
	)
	request.Header.Set("Authorization", "Bearer secret")
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	lifecycle.SetFinalRequest(request)
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent, Committed: true,
	}, time.Unix(2, 0))
	if !executor.RegisterComposite(request) {
		t.Fatal("RegisterComposite() = false")
	}
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if global.calls != 1 || route.calls != 1 {
		t.Fatalf("sanitizer calls = global:%d route:%d, want 1/1", global.calls, route.calls)
	}
}

func TestMetadataLoggerPreservesExactOwnerAndServerlessResponseMethods(t *testing.T) {
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	logger := &metadataLogContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "http-logger",
	}}
	wrapper, err := newMetadataPlugin("http-logger", logger, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(http-logger) error = %v", err)
	}
	if _, ok := wrapper.(base.LogPhasePlugin); !ok {
		t.Fatalf("metadata http-logger lost log phase: %T", wrapper)
	}
	if _, ok := wrapper.(base.SnapshotFinalizerPlugin); ok {
		t.Fatalf("metadata http-logger gained snapshot finalizer: %T", wrapper)
	}

	serverless := &metadataLogResponseContractPlugin{
		metadataLogContractPlugin: metadataLogContractPlugin{
			metadataResponseContractPlugin: metadataResponseContractPlugin{
				name:       "serverless-pre-function",
				descriptor: base.BindingPhaseDescriptor{RequestStage: "none", Header: true},
			},
		},
		config: metadataPhaseConfig{descriptor: base.BindingPhaseDescriptor{
			RequestStage: "none",
			Header:       true,
		}},
	}
	wrapper, err = newMetadataPlugin("serverless-pre-function", serverless, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(serverless-pre-function) error = %v", err)
	}
	header, ok := wrapper.(base.HeaderFilterPlugin)
	if !ok {
		t.Fatalf("metadata serverless header phase lost header callback: %T", wrapper)
	}
	disabled := httptest.NewRequest(http.MethodGet, "/?enabled=no", nil)
	if err := header.RunHeaderFilter(disabled, &base.ResponseState{Status: http.StatusOK}); err != nil {
		t.Fatalf("filtered header callback error = %v", err)
	}
	enabled := httptest.NewRequest(http.MethodGet, "/?enabled=yes", nil)
	if err := header.RunHeaderFilter(enabled, &base.ResponseState{Status: http.StatusOK}); err != nil {
		t.Fatalf("header callback error = %v", err)
	}
	if serverless.headerCalls != 1 {
		t.Fatalf("serverless header calls = %d, want 1", serverless.headerCalls)
	}

	serverless.config = metadataPhaseConfig{descriptor: base.BindingPhaseDescriptor{
		RequestStage: "none",
		Log:          true,
	}}
	wrapper, err = newMetadataPlugin("serverless-pre-function", serverless, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(serverless log) error = %v", err)
	}
	logPhase, ok := wrapper.(base.LogPhasePlugin)
	if !ok {
		t.Fatalf("metadata serverless log phase lost log callback: %T", wrapper)
	}
	snapshot := base.LogSnapshot{}
	snapshot.Request.Query = url.Values{"enabled": {"yes"}}
	if err := logPhase.RunLogPhase(snapshot); err != nil {
		t.Fatalf("serverless log callback error = %v", err)
	}
	if serverless.logCalls != 1 {
		t.Fatalf("serverless log calls = %d, want 1", serverless.logCalls)
	}
}
