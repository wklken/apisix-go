package api_breaker

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestAPIBreakerFinalizerRejectsMalformedUpstreamStatus(t *testing.T) {
	config := Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	}
	for _, value := range []any{"502, invalid", float64(500), 0, 1000} {
		plugin := newTestPlugin(t, config)
		observeAPIBreakerUpstreamValue(
			t, plugin, "http://example.test/anything", value, http.StatusInternalServerError,
		)
		assertAPIBreakerDecision(
			t, plugin, "http://example.test/anything", base.RequestContinue,
		)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerResolvesBreakResponseHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusTooManyRequests,
		BreakResponseBody: new("blocked"),
		BreakResponseHeaders: []Header{
			{Key: "X-Break-Method", Value: "$request_method"},
			{Key: "X-Break-URI", Value: "$request_uri"},
			{Key: "X-Break-IP", Value: "$remote_addr"},
		},
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})

	nextCalls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/blocked?first=1", nil)
	handler.ServeHTTP(first, firstReq)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first response code = %d, want %d", first.Code, http.StatusInternalServerError)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/blocked?x=1", nil)
	secondReq.RemoteAddr = "192.0.2.10:12345"
	handler.ServeHTTP(second, secondReq)
	result := second.Result()
	defer func() { _ = result.Body.Close() }()

	if result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked response code = %d, want %d", result.StatusCode, http.StatusTooManyRequests)
	}
	if nextCalls != 1 {
		t.Fatalf("next calls = %d, want only the first request to reach upstream", nextCalls)
	}
	if got := result.Header.Get("X-Break-Method"); got != http.MethodPost {
		t.Fatalf("X-Break-Method = %q, want %q", got, http.MethodPost)
	}
	if got := result.Header.Get("X-Break-URI"); got != "/blocked?x=1" {
		t.Fatalf("X-Break-URI = %q, want /blocked?x=1", got)
	}
	if got := result.Header.Get("X-Break-IP"); got != "192.0.2.10" {
		t.Fatalf("X-Break-IP = %q, want 192.0.2.10", got)
	}
}

func TestHandlerHealthySuccessesClearAccumulatedFailures(t *testing.T) {
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		MaxBreakerSec:     10,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(3),
		},
		Healthy: HealthCheck{
			HTTPStatuses: []int{http.StatusNoContent},
			Successes:    new(3),
		},
	})

	statuses := []int{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusNoContent,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusNoContent,
	}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := statuses[0]
		statuses = statuses[1:]
		w.WriteHeader(status)
	}))

	wants := []int{500, 500, 204, 204, 204, 500, 500, 204}
	for i, want := range wants {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api", nil))
		if response.Code != want {
			t.Fatalf("response %d code = %d, want %d", i+1, response.Code, want)
		}
	}
}

func TestHandlerUsesExponentialBreakerWindowCappedByConfiguration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	p := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusBadGateway,
		MaxBreakerSec:     10,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	p.now = func() time.Time { return now }
	upstreamCalls := 0
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))

	request := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api", nil))
		if response.Code != want {
			t.Fatalf("response code = %d, want %d at %s", response.Code, want, now)
		}
	}
	request(500)
	now = now.Add(time.Second)
	request(502)
	now = now.Add(1100 * time.Millisecond)
	request(500)
	now = now.Add(3 * time.Second)
	request(502)
	now = now.Add(1100 * time.Millisecond)
	request(500)
	now = now.Add(8100 * time.Millisecond)
	request(500)
	if upstreamCalls != 4 {
		t.Fatalf("upstream calls = %d, want 4", upstreamCalls)
	}
}

func TestBreakerTimeLogIsDebugLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	entries := make(chan logger.Entry, 4)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if strings.Contains(entry.Message, "breaker_time") {
			entries <- entry
		}
	})
	t.Cleanup(stop)

	failures := 2
	p := newTestPlugin(t, Config{
		Unhealthy:     UnHealthCheck{Failures: &failures},
		MaxBreakerSec: 300,
	})
	key := breakerKey{host: "example.test", uri: "/"}
	p.observeStatus(key, http.StatusInternalServerError)
	p.observeStatus(key, http.StatusInternalServerError)

	_ = p.shouldBreak(key)
	select {
	case entry := <-entries:
		t.Fatalf("breaker_time logged at info level: %q", entry.Message)
	case <-time.After(100 * time.Millisecond):
	}

	if err := logger.ConfigureLevel("debug"); err != nil {
		t.Fatalf("configure debug level: %v", err)
	}
	_ = p.shouldBreak(key)
	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "breaker_time") {
			t.Fatalf("debug entry = %q, want breaker_time", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("breaker_time not logged at debug level")
	}
}

func TestAPIBreakerSharesStateByHostAndURIAcrossPluginInstances(t *testing.T) {
	state := NewState()
	config := Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	}
	first := newTestPlugin(t, config)
	second := newTestPlugin(t, config)
	first.SetState(state)
	second.SetState(state)
	now := time.Unix(1_700_000_000, 0)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }

	observeAPIBreakerUpstreamStatus(
		t, first, "http://Example.TEST:9080/api?first=1", http.StatusInternalServerError, http.StatusOK,
	)
	assertAPIBreakerDecision(
		t, second, "http://example.test/api?second=2", base.RequestStop,
	)
	assertAPIBreakerDecision(
		t, second, "http://other.example.test/api", base.RequestContinue,
	)
	assertAPIBreakerDecision(
		t, second, "http://example.test/other", base.RequestContinue,
	)
}

func TestAPIBreakerRecoveryIsKeyScoped(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
		Healthy: HealthCheck{
			HTTPStatuses: []int{http.StatusOK},
			Successes:    new(1),
		},
	})
	now := time.Unix(1_700_000_000, 0)
	plugin.now = func() time.Time { return now }
	first := breakerKey{host: "example.test", uri: "/first"}
	second := breakerKey{host: "example.test", uri: "/second"}
	plugin.observeStatus(first, http.StatusInternalServerError)
	plugin.observeStatus(second, http.StatusInternalServerError)
	plugin.observeStatus(first, http.StatusOK)

	if plugin.shouldBreak(first) {
		t.Fatal("healthy recovery retained the recovered key")
	}
	if !plugin.shouldBreak(second) {
		t.Fatal("healthy recovery cleared an unrelated key")
	}
}

func TestAPIBreakerWindowExpiryAllowsConcurrentTrials(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	now := time.Unix(1_700_000_000, 0)
	plugin.now = func() time.Time { return now }
	key := breakerKey{host: "example.test", uri: "/api"}
	plugin.observeStatus(key, http.StatusInternalServerError)
	now = now.Add(3 * time.Second)

	start := make(chan struct{})
	results := make(chan bool, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			results <- plugin.shouldBreak(key)
		})
	}
	close(start)
	wait.Wait()
	close(results)
	for blocked := range results {
		if blocked {
			t.Fatal("expired breaker window admitted only one trial")
		}
	}
}

func TestAPIBreakerLastTimeExpiresUsingWritingConfiguration(t *testing.T) {
	state := NewState()
	now := time.Unix(1_700_000_000, 0)
	writer := newTestPlugin(t, Config{
		MaxBreakerSec: 3,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	reader := newTestPlugin(t, Config{
		MaxBreakerSec: 300,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	writer.SetState(state)
	reader.SetState(state)
	writer.now = func() time.Time { return now }
	reader.now = func() time.Time { return now }
	key := breakerKey{host: "example.test", uri: "/api"}
	for range 4 {
		writer.observeStatus(key, http.StatusInternalServerError)
	}
	if !reader.shouldBreak(key) {
		t.Fatal("shared lasttime did not block before the writing configuration TTL")
	}
	now = now.Add(3 * time.Second)
	if reader.shouldBreak(key) {
		t.Fatal("shared lasttime outlived the writing configuration TTL")
	}
}

func TestAPIBreakerStateOwnsStoredKeyBytes(t *testing.T) {
	state := newStateWithBudget(1024)
	key := breakerKey{
		host: strings.Repeat("host", 8),
		uri:  "/" + strings.Repeat("path", 16),
	}
	state.mu.Lock()
	entry := state.insertLocked(key)
	var stored breakerKey
	for stored = range state.entries {
		break
	}
	state.mu.Unlock()
	if entry == nil {
		t.Fatal("breaker state rejected a key within budget")
	}
	if unsafe.StringData(stored.host) == unsafe.StringData(key.host) {
		t.Fatal("stored host retained the request key backing bytes")
	}
	if unsafe.StringData(stored.uri) == unsafe.StringData(key.uri) {
		t.Fatal("stored URI retained the request key backing bytes")
	}
}

func TestAPIBreakerStateEvictsWithinMemoryBudget(t *testing.T) {
	const budget = 512
	state := newStateWithBudget(budget)
	plugin := newTestPlugin(t, Config{
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	plugin.SetState(state)
	now := time.Unix(1_700_000_000, 0)
	plugin.now = func() time.Time { return now }
	first := breakerKey{host: "example.test", uri: "/" + strings.Repeat("a", 64)}
	plugin.observeStatus(first, http.StatusInternalServerError)
	var latest breakerKey
	for index := range 32 {
		latest = breakerKey{
			host: "example.test",
			uri:  "/" + strconv.Itoa(index) + strings.Repeat("x", 64),
		}
		plugin.observeStatus(latest, http.StatusInternalServerError)
	}
	if state.usedBytes > budget {
		t.Fatalf("state used bytes = %d, budget %d", state.usedBytes, budget)
	}
	if plugin.shouldBreak(first) {
		t.Fatal("oldest breaker key survived bounded-state eviction")
	}
	if !plugin.shouldBreak(latest) {
		t.Fatal("newest breaker key was not retained after bounded-state eviction")
	}
}

func TestAPIBreakerFinalizerUsesLastUpstreamStatus(t *testing.T) {
	config := Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	}

	t.Run("last retry status trips despite rewritten final success", func(t *testing.T) {
		plugin := newTestPlugin(t, config)
		observeAPIBreakerUpstreamValue(t, plugin, "http://example.test/api", "502, 500", http.StatusOK)
		assertAPIBreakerDecision(t, plugin, "http://example.test/api", base.RequestStop)
	})

	t.Run("healthy upstream is not replaced by final failure", func(t *testing.T) {
		plugin := newTestPlugin(t, config)
		observeAPIBreakerUpstreamStatus(
			t, plugin, "http://example.test/api", http.StatusOK, http.StatusInternalServerError,
		)
		assertAPIBreakerDecision(t, plugin, "http://example.test/api", base.RequestContinue)
	})

	t.Run("local failure without upstream is ignored", func(t *testing.T) {
		plugin := newTestPlugin(t, config)
		observeAPIBreakerUpstreamValue(
			t, plugin, "http://example.test/api", nil, http.StatusInternalServerError,
		)
		assertAPIBreakerDecision(t, plugin, "http://example.test/api", base.RequestContinue)
	})
}

func TestAPIBreakerCapturesKeyBeforeDownstreamRequestMutation(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		BreakResponseCode: http.StatusServiceUnavailable,
		Unhealthy: UnHealthCheck{
			HTTPStatuses: []int{http.StatusInternalServerError},
			Failures:     new(1),
		},
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.test/original", nil)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	result := plugin.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	result.Request.Host = "rewritten.example.test"
	result.Request.URL.Path = "/rewritten"
	apisixctx.RegisterRequestVar(result.Request, "$upstream_status", http.StatusInternalServerError)
	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK, Committed: true,
	})
	lifecycle.Finalize()

	assertAPIBreakerDecision(t, plugin, "http://example.test/original", base.RequestStop)
	assertAPIBreakerDecision(t, plugin, "http://rewritten.example.test/rewritten", base.RequestContinue)
}

func observeAPIBreakerUpstreamStatus(
	t *testing.T,
	plugin *Plugin,
	target string,
	upstreamStatus int,
	finalStatus int,
) {
	t.Helper()
	observeAPIBreakerUpstreamValue(t, plugin, target, upstreamStatus, finalStatus)
}

func observeAPIBreakerUpstreamValue(
	t *testing.T,
	plugin *Plugin,
	target string,
	upstreamStatus any,
	finalStatus int,
) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	result := plugin.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	if upstreamStatus != nil {
		apisixctx.RegisterRequestVar(result.Request, "$upstream_status", upstreamStatus)
	}
	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: finalStatus, Committed: true,
	})
	lifecycle.Finalize()
}

func assertAPIBreakerDecision(t *testing.T, plugin *Plugin, target string, want base.RequestDecision) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
	result := plugin.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != want {
		t.Fatalf("RunRequestPhase(%q) decision = %v, want %v", target, result.Decision, want)
	}
}
