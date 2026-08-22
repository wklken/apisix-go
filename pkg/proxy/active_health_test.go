package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestParseActiveHealthConfigDefaultsWhenActiveAbsent(t *testing.T) {
	config, enabled, err := ParseActiveHealthConfig(map[string]any{})
	if err != nil {
		t.Fatalf("ParseActiveHealthConfig() error = %v", err)
	}
	if enabled {
		t.Fatal("ParseActiveHealthConfig() enabled = true without checks.active")
	}
	if config.Type != "" || config.HTTPPath != "" || config.Concurrency != 0 {
		t.Fatalf("ParseActiveHealthConfig() = %#v, want zero config", config)
	}
}

func TestParseActiveHealthConfigParsesOfficialShape(t *testing.T) {
	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{
			"type":        "https",
			"http_path":   "/healthz",
			"host":        "orders.example.com",
			"timeout":     2,
			"concurrency": 4,
			"healthy": map[string]any{
				"interval":      3,
				"successes":     2,
				"http_statuses": []any{200, 201},
			},
			"unhealthy": map[string]any{
				"interval":      2,
				"http_failures": 3,
				"tcp_failures":  1,
				"timeouts":      4,
				"http_statuses": []any{500, 503},
			},
		},
	})
	if err != nil {
		t.Fatalf("ParseActiveHealthConfig() error = %v", err)
	}
	if !enabled {
		t.Fatal("ParseActiveHealthConfig() enabled = false for a full block")
	}
	if config.Type != "https" || config.HTTPPath != "/healthz" || config.Host != "orders.example.com" {
		t.Fatalf("active endpoint = %s %s host=%s", config.Type, config.HTTPPath, config.Host)
	}
	if config.Timeout != 2*time.Second || config.Concurrency != 4 {
		t.Fatalf("active timeout/concurrency = %s/%d", config.Timeout, config.Concurrency)
	}
	if config.HealthyInterval != 3*time.Second || config.HealthySuccesses != 2 {
		t.Fatalf("active healthy = %s/%d", config.HealthyInterval, config.HealthySuccesses)
	}
	if config.UnhealthyInterval != 2*time.Second || config.UnhealthyHTTPFails != 3 ||
		config.UnhealthyTCPFails != 1 || config.UnhealthyTimeouts != 4 {
		t.Fatalf(
			"active unhealthy = %s/%d/%d/%d",
			config.UnhealthyInterval,
			config.UnhealthyHTTPFails,
			config.UnhealthyTCPFails,
			config.UnhealthyTimeouts,
		)
	}
	if len(config.HealthyStatuses) != 2 || len(config.UnhealthyStatuses) != 2 {
		t.Fatalf("active status sets = %v/%v", config.HealthyStatuses, config.UnhealthyStatuses)
	}
}

func TestParseActiveHealthConfigUsesAPISIXActiveDefaults(t *testing.T) {
	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{},
	})
	if err != nil {
		t.Fatalf("ParseActiveHealthConfig() error = %v", err)
	}
	if !enabled {
		t.Fatal("ParseActiveHealthConfig() enabled = false for an active block")
	}
	if config.UnhealthyHTTPFails != 5 || config.UnhealthyTCPFails != 2 || config.UnhealthyTimeouts != 3 {
		t.Fatalf(
			"active failure defaults = HTTP:%d TCP:%d timeout:%d, want 5/2/3",
			config.UnhealthyHTTPFails,
			config.UnhealthyTCPFails,
			config.UnhealthyTimeouts,
		)
	}
	wantHealthy := map[int]struct{}{http.StatusOK: {}, http.StatusFound: {}}
	if !reflect.DeepEqual(config.HealthyStatuses, wantHealthy) {
		t.Fatalf("active healthy statuses = %#v, want %#v", config.HealthyStatuses, wantHealthy)
	}
}

func TestActiveHealthProbeClassifiesHTTPTransportAndTimeoutFailures(t *testing.T) {
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer statusServer.Close()

	newChecker := func(transport http.RoundTripper) *activeHealthChecker {
		return &activeHealthChecker{
			config: ActiveHealthConfig{
				Type:     "http",
				HTTPPath: "/",
				Timeout:  time.Second,
				HealthyStatuses: map[int]struct{}{
					http.StatusOK: {},
				},
				UnhealthyStatuses: map[int]struct{}{
					http.StatusInternalServerError: {},
				},
			},
			ctx:        context.Background(),
			httpClient: &http.Client{Transport: transport},
		}
	}

	statusChecker := newChecker(http.DefaultTransport)
	if got := statusChecker.probeResult(statusServer.URL); got != activeProbeHTTPFailure {
		t.Fatalf("HTTP failure classification = %v, want HTTP failure", got)
	}

	transportChecker := newChecker(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset by peer")
	}))
	if got := transportChecker.probeResult("http://upstream.example/"); got != activeProbeTCPFailure {
		t.Fatalf("transport failure classification = %v, want TCP failure", got)
	}

	timeoutChecker := newChecker(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	}))
	if got := timeoutChecker.probeResult("http://upstream.example/"); got != activeProbeTimeout {
		t.Fatalf("timeout classification = %v, want timeout", got)
	}
}

func TestActiveHealthCheckerUsesTCPThresholdForHTTPTransportFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	target := upstream.URL
	upstream.Close()

	lb, err := NewHealthAwareLoadBalance(map[string]int{target: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checker := newActiveHealthChecker(
		ActiveHealthConfig{
			Type:               "http",
			HTTPPath:           "/",
			Timeout:            20 * time.Millisecond,
			Concurrency:        1,
			HealthyInterval:    time.Millisecond,
			UnhealthyInterval:  time.Millisecond,
			HealthySuccesses:   1,
			UnhealthyHTTPFails: 10,
			UnhealthyTCPFails:  1,
			UnhealthyTimeouts:  10,
			HealthyStatuses:    map[int]struct{}{http.StatusOK: {}},
			UnhealthyStatuses:  map[int]struct{}{http.StatusInternalServerError: {}},
		},
		lb,
		map[string]int{target: 1},
		"active-orders",
		NopClusterObserver{},
		http.DefaultTransport,
	)
	checker.Start()
	t.Cleanup(checker.Close)

	deadline := time.Now().Add(time.Second)
	for lb.IsHealthy(target) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if lb.IsHealthy(target) {
		t.Fatal("TCP threshold was not applied to an HTTP transport failure")
	}
}

func TestActiveHealthCheckerUsesTimeoutThresholdForHTTPProbeTimeouts(t *testing.T) {
	const target = "http://upstream.example/"
	lb, err := NewHealthAwareLoadBalance(map[string]int{target: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	checker := newActiveHealthChecker(
		ActiveHealthConfig{
			Type:               "http",
			HTTPPath:           "/",
			Timeout:            time.Second,
			Concurrency:        1,
			HealthyInterval:    time.Millisecond,
			UnhealthyInterval:  time.Millisecond,
			HealthySuccesses:   1,
			UnhealthyHTTPFails: 10,
			UnhealthyTCPFails:  10,
			UnhealthyTimeouts:  1,
			HealthyStatuses:    map[int]struct{}{http.StatusOK: {}},
			UnhealthyStatuses:  map[int]struct{}{http.StatusInternalServerError: {}},
		},
		lb,
		map[string]int{target: 1},
		"active-orders",
		NopClusterObserver{},
		transport,
	)
	checker.Start()
	t.Cleanup(checker.Close)

	deadline := time.Now().Add(time.Second)
	for lb.IsHealthy(target) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if lb.IsHealthy(target) {
		t.Fatal("timeout threshold was not applied to an HTTP probe timeout")
	}
}

func TestParseActiveHealthConfigRejectsMalformedFields(t *testing.T) {
	for _, test := range []struct {
		name  string
		block map[string]any
		want  string
	}{
		{
			name:  "bad type",
			block: map[string]any{"type": "udp"},
			want:  "checks.active.type",
		},
		{
			name:  "empty path",
			block: map[string]any{"http_path": ""},
			want:  "checks.active.http_path",
		},
		{
			name:  "negative timeout",
			block: map[string]any{"timeout": -1},
			want:  "checks.active.timeout",
		},
		{
			name:  "negative concurrency",
			block: map[string]any{"concurrency": 0},
			want:  "checks.active.concurrency",
		},
		{
			name:  "bad healthy interval",
			block: map[string]any{"healthy": map[string]any{"interval": "fast"}},
			want:  "checks.active.healthy.interval",
		},
		{
			name:  "zero healthy successes",
			block: map[string]any{"healthy": map[string]any{"successes": 0}},
			want:  "checks.active.healthy.successes",
		},
		{
			name:  "negative unhealthy interval",
			block: map[string]any{"unhealthy": map[string]any{"interval": -2}},
			want:  "checks.active.unhealthy.interval",
		},
		{
			name:  "bad unhealthy status",
			block: map[string]any{"unhealthy": map[string]any{"http_statuses": []any{700}}},
			want:  "checks.active.unhealthy.http_statuses",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := ParseActiveHealthConfig(map[string]any{"active": test.block})
			if err == nil {
				t.Fatalf("ParseActiveHealthConfig() error = nil, want %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseActiveHealthConfig() error = %q, want it to name %q", err, test.want)
			}
		})
	}
}

func TestActiveHealthCheckerRecoversAfterConsecutiveSuccesses(t *testing.T) {
	healthyResponses := atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if healthyResponses.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{
			"http_path": "/healthz",
			"healthy": map[string]any{
				"interval":  1,
				"successes": 2,
			},
			"unhealthy": map[string]any{
				"interval":      1,
				"http_failures": 1,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("active health config disabled")
	}
	lb, err := NewHealthAwareLoadBalance(map[string]int{upstream.URL: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checker := newActiveHealthChecker(
		config,
		lb,
		map[string]int{upstream.URL: 1},
		"active-orders",
		NopClusterObserver{},
		http.DefaultTransport,
	)
	checker.Start()
	t.Cleanup(checker.Close)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if lb.IsHealthy(upstream.URL) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("active probe did not recover the target")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestActiveHealthCheckerQuarantinesConfiguredHTTPFailure(t *testing.T) {
	failures := atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failures.Add(1) <= 2 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{
			"http_path": "/healthz",
			"healthy": map[string]any{
				"interval":  1,
				"successes": 1,
			},
			"unhealthy": map[string]any{
				"interval":      1,
				"http_failures": 1,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("active health config disabled")
	}
	lb, err := NewHealthAwareLoadBalance(map[string]int{upstream.URL: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checker := newActiveHealthChecker(
		config,
		lb,
		map[string]int{upstream.URL: 1},
		"active-orders",
		NopClusterObserver{},
		http.DefaultTransport,
	)
	checker.Start()
	t.Cleanup(checker.Close)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if !lb.IsHealthy(upstream.URL) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("active probe did not quarantine the failing target")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestActiveHealthCheckerCloseStopsProbeGoroutines(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{
			"http_path": "/healthz",
			"healthy":   map[string]any{"interval": 1, "successes": 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("active health config disabled")
	}
	lb, err := NewHealthAwareLoadBalance(map[string]int{upstream.URL: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checker := newActiveHealthChecker(
		config,
		lb,
		map[string]int{upstream.URL: 1},
		"active-orders",
		NopClusterObserver{},
		http.DefaultTransport,
	)
	checker.Start()
	checker.Close()
}

func TestActiveHealthCheckerUsesConfiguredHTTPSProbeScheme(t *testing.T) {
	requestSeen := make(chan struct {
		tls  bool
		path string
		host string
	}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSeen <- struct {
			tls  bool
			path string
			host string
		}{tls: r.TLS != nil, path: r.URL.Path, host: r.Host}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	config, enabled, err := ParseActiveHealthConfig(map[string]any{
		"active": map[string]any{
			"type":      "https",
			"http_path": "/healthz",
			"host":      "orders.example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Fatal("active health config disabled")
	}

	target := "http://" + strings.TrimPrefix(upstream.URL, "https://")
	lb, err := NewHealthAwareLoadBalance(map[string]int{target: 1}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	checker := newActiveHealthChecker(
		config,
		lb,
		map[string]int{target: 1},
		"active-orders",
		NopClusterObserver{},
		upstream.Client().Transport,
	)
	if !checker.probeOnce(target) {
		t.Fatal("HTTPS active probe failed against an http target identity")
	}

	seen := <-requestSeen
	if !seen.tls {
		t.Fatal("active probe did not use TLS")
	}
	if seen.path != "/healthz" {
		t.Fatalf("active probe path = %q, want /healthz", seen.path)
	}
	if seen.host != "orders.example.com" {
		t.Fatalf("active probe Host = %q, want orders.example.com", seen.host)
	}
}
