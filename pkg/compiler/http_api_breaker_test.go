package compiler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/generation"
	api_breaker "github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestPreparedHTTPAPIBreakerSharesStateAcrossRoutes(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	pluginConfig := `"api-breaker":{"break_response_code":503,"unhealthy":{"http_statuses":[500],"failures":1}}`
	snapshot := mustGenerationSnapshot(t, 41, []generation.Resource{
		resourceValue("routes", "get", fmt.Sprintf(
			`{"id":"get","uri":"/api","methods":["GET"],"plugins":{%s},"upstream":{"type":"roundrobin","nodes":{"%s":1}}}`,
			pluginConfig,
			upstreamAddress,
		)),
		resourceValue("routes", "post", fmt.Sprintf(
			`{"id":"post","uri":"/api","methods":["POST"],"plugins":{%s},"upstream":{"type":"roundrobin","nodes":{"%s":1}}}`,
			pluginConfig,
			upstreamAddress,
		)),
	}, nil)
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"api-breaker"}
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(snapshot, generation.DomainHTTP),
		snapshot,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	if quarantined := prepared.HTTP().Quarantined(); len(quarantined) != 0 {
		t.Fatalf("api-breaker routes quarantined: %#v", quarantined)
	}

	first := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		prepared.HTTP().Handler(),
		first,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.test/api", nil),
	)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response = %d, want upstream 500", first.Code)
	}
	second := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		prepared.HTTP().Handler(),
		second,
		httptest.NewRequest(http.MethodPost, "http://gateway.example.test/api", nil),
	)
	if second.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 1 {
		t.Fatalf("second response/upstream calls = %d/%d, want 503/1", second.Code, upstreamCalls.Load())
	}
}

func TestPreparedHTTPAPIBreakerUsesUpstreamStatusBeforeResponseRewrite(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	pluginConfig := `"api-breaker":{"break_response_code":503,"unhealthy":{"http_statuses":[500],"failures":1}}`
	snapshot := mustGenerationSnapshot(t, 42, []generation.Resource{
		resourceValue("routes", "rewritten-get", fmt.Sprintf(
			`{"id":"rewritten-get","uri":"/api","methods":["GET"],"plugins":{%s,"response-rewrite":{"status_code":200}},"upstream":{"type":"roundrobin","nodes":{"%s":1}}}`,
			pluginConfig,
			upstreamAddress,
		)),
		resourceValue("routes", "breaker-post", fmt.Sprintf(
			`{"id":"breaker-post","uri":"/api","methods":["POST"],"plugins":{%s},"upstream":{"type":"roundrobin","nodes":{"%s":1}}}`,
			pluginConfig,
			upstreamAddress,
		)),
	}, nil)
	prepared := prepareAPIBreakerGeneration(t, snapshot, []string{"api-breaker", "response-rewrite"})

	first := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		prepared.HTTP().Handler(),
		first,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.test/api", nil),
	)
	if first.Code != http.StatusOK {
		t.Fatalf("rewritten response = %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		prepared.HTTP().Handler(),
		second,
		httptest.NewRequest(http.MethodPost, "http://gateway.example.test/api", nil),
	)
	if second.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 1 {
		t.Fatalf("breaker response/upstream calls = %d/%d, want 503/1", second.Code, upstreamCalls.Load())
	}
}

func TestPreparedHTTPAPIBreakerStateSurvivesGenerationHandoff(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	upstreamAddress := strings.TrimPrefix(upstream.URL, "http://")
	snapshot := func(revision uint64) generation.Snapshot {
		return mustGenerationSnapshot(t, revision, []generation.Resource{
			resourceValue("routes", "api-breaker", fmt.Sprintf(
				`{"id":"api-breaker","uri":"/api","plugins":{"api-breaker":{"break_response_code":503,"unhealthy":{"http_statuses":[500],"failures":1}}},"upstream":{"type":"roundrobin","nodes":{"%s":1}}}`,
				upstreamAddress,
			)),
		}, nil)
	}
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"api-breaker"}
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	prepare := func(revision uint64) *PreparedGeneration {
		t.Helper()
		desired := snapshot(revision)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		return prepared
	}
	firstGeneration := prepare(43)
	secondGeneration := prepare(44)
	t.Cleanup(func() { _ = secondGeneration.Close(context.Background()) })

	first := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		firstGeneration.HTTP().Handler(),
		first,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.test/api", nil),
	)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first generation response = %d, want 500", first.Code)
	}
	if err := firstGeneration.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	second := httptest.NewRecorder()
	servePreparedHTTPAndFinalize(
		t,
		secondGeneration.HTTP().Handler(),
		second,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.test/api", nil),
	)
	if second.Code != http.StatusServiceUnavailable || upstreamCalls.Load() != 1 {
		t.Fatalf("handoff response/upstream calls = %d/%d, want 503/1", second.Code, upstreamCalls.Load())
	}
}

func prepareAPIBreakerGeneration(
	t *testing.T,
	snapshot generation.Snapshot,
	enabledPlugins []string,
) *PreparedGeneration {
	t.Helper()
	factory := newScopedWorkerTestFactory(t)
	factory.effective.Config.Plugins = enabledPlugins
	t.Cleanup(func() { _ = factory.Close(context.Background()) })
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(snapshot, generation.DomainHTTP),
		snapshot,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	if quarantined := prepared.HTTP().Quarantined(); len(quarantined) != 0 {
		t.Fatalf("api-breaker routes quarantined: %#v", quarantined)
	}
	return prepared
}

func servePreparedHTTPAndFinalize(
	t *testing.T,
	handler http.Handler,
	response *httptest.ResponseRecorder,
	request *http.Request,
) {
	t.Helper()
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	handler.ServeHTTP(response, request)
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code, Committed: true,
	}, time.Now())
	if finalization := lifecycle.FinalizeResult(); len(finalization.Failures) != 0 || finalization.FatalPanic != nil {
		t.Fatalf("request finalization = %#v, want success", finalization)
	}
}

func TestHTTPAPIBreakerStateOutlivesCreatingGenerationWhileReused(t *testing.T) {
	sharedResources := runtime.NewResourceRegistry()
	first, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	second, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	first.registry, second.registry = sharedResources, sharedResources

	firstState, err := first.acquireHTTPAPIBreakerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := second.acquireHTTPAPIBreakerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstState == nil || firstState != secondState || sharedResources.Len() != 1 {
		t.Fatalf("shared api-breaker state = %p/%p registry=%d", firstState, secondState, sharedResources.Len())
	}

	tripAPIBreakerState(t, firstState, "http://example.test/api")
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sharedResources.Len() != 1 {
		t.Fatalf("registry after first retirement = %d, want 1", sharedResources.Len())
	}
	assertAPIBreakerStateBlocks(t, secondState, "http://example.test/api", true)

	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sharedResources.Len() != 0 {
		t.Fatalf("registry after final retirement = %d, want 0", sharedResources.Len())
	}
	assertAPIBreakerStateBlocks(t, secondState, "http://example.test/api", false)
}

func TestHTTPAPIBreakerStateRollbackReleasesTentativeLease(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	checkpoint, err := prepared.cleanup.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	state, err := prepared.acquireHTTPAPIBreakerState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.cleanup.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if fixture.registry.Len() != 0 {
		t.Fatalf("registry after rollback = %d, want 0", fixture.registry.Len())
	}
	assertAPIBreakerStateBlocks(t, state, "http://example.test/api", false)
}

func tripAPIBreakerState(t *testing.T, state *api_breaker.State, target string) {
	t.Helper()
	plugin := newCompilerAPIBreaker(t, state)
	response := httptest.NewRecorder()
	plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("trip response = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func assertAPIBreakerStateBlocks(
	t *testing.T,
	state *api_breaker.State,
	target string,
	wantBlocked bool,
) {
	t.Helper()
	plugin := newCompilerAPIBreaker(t, state)
	response := httptest.NewRecorder()
	plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	wantStatus := http.StatusInternalServerError
	if wantBlocked {
		wantStatus = http.StatusServiceUnavailable
	}
	if response.Code != wantStatus {
		t.Fatalf("blocked=%t response = %d, want %d", wantBlocked, response.Code, wantStatus)
	}
}

func newCompilerAPIBreaker(t *testing.T, state *api_breaker.State) *api_breaker.Plugin {
	t.Helper()
	plugin := &api_breaker.Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatal(err)
	}
	config := plugin.Config().(*api_breaker.Config)
	config.BreakResponseCode = http.StatusServiceUnavailable
	config.Unhealthy.Failures = new(1)
	plugin.SetState(state)
	if err := plugin.PostInit(); err != nil {
		t.Fatal(err)
	}
	return plugin
}
