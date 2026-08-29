package route

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type preparedTrafficSplitAcquirer struct {
	t     testing.TB
	route resource.Route
	ssls  map[string]resource.SSL
}

func (a preparedTrafficSplitAcquirer) Acquire(
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
) (*traffic_split.Runtime, error) {
	config, err := PlanTrafficSplitCluster(
		a.route, upstream, targets, priorities, a.ssls, &testEffectiveConfig().Config,
	)
	if err != nil {
		return nil, err
	}
	cluster, err := pxy.NewCluster(config, pxy.NopClusterObserver{})
	if err != nil {
		return nil, err
	}
	a.t.Cleanup(cluster.Close)
	return &traffic_split.Runtime{
		LoadBalancer: cluster.LoadBalancer(), RoundTripper: cluster.RoundTripper(),
	}, nil
}

func testTrafficSplitBinding(t testing.TB, routeResource resource.Route) plugin.Binding {
	t.Helper()
	instance := plugin.New("traffic-split", base.Dependencies{})
	if instance == nil {
		t.Fatal("traffic-split plugin is unavailable")
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("traffic-split Init() error = %v", err)
	}
	if err := util.Parse(routeResource.Plugins["traffic-split"], instance.Config()); err != nil {
		t.Fatalf("traffic-split config error = %v", err)
	}
	instance.(*traffic_split.Plugin).SetRuntimeAcquirer(preparedTrafficSplitAcquirer{t: t, route: routeResource})
	if err := instance.PostInit(); err != nil {
		t.Fatalf("traffic-split PostInit() error = %v", err)
	}
	if stopper, ok := instance.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	binding, err := plugin.BindPluginChecked(
		"traffic-split",
		instance,
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(traffic-split) error = %v", err)
	}
	return binding
}

func TestApplyTrafficSplitOverrideDefaultsToPassHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme: "https",
		Host:   "shadow.example.com:9443",
	})

	applyTrafficSplitOverride(req)

	if req.URL.Scheme != "https" {
		t.Fatalf("scheme = %q, want https", req.URL.Scheme)
	}
	if req.URL.Host != "shadow.example.com:9443" {
		t.Fatalf("URL host = %q, want shadow.example.com:9443", req.URL.Host)
	}
	if req.Host != "route.example.com" {
		t.Fatalf("Host = %q, want route.example.com", req.Host)
	}
}

func TestTrafficSplitRouteUsesSelectedUpstreamMTLS(t *testing.T) {
	serverCertificate, clientCertificate, clientKey, clientCAs := routeMTLSCertificates(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	upstream.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		ClientCAs:    clientCAs,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse mTLS upstream URL: %v", err)
	}
	port, err := strconv.Atoi(targetURL.Port())
	if err != nil {
		t.Fatalf("parse mTLS upstream port: %v", err)
	}

	route := resource.Route{
		ID:  "traffic-split-mtls",
		Uri: "/split",
		Plugins: map[string]resource.PluginConfig{
			"traffic-split": map[string]any{
				"rules": []any{map[string]any{
					"weighted_upstreams": []any{map[string]any{
						"weight": 1,
						"upstream": map[string]any{
							"scheme": "https",
							"tls": map[string]any{
								"client_cert": clientCertificate,
								"client_key":  clientKey,
								"verify":      false,
							},
							"nodes": []any{map[string]any{
								"host":   targetURL.Hostname(),
								"port":   port,
								"weight": 1,
							}},
						},
					}},
				}},
			},
		},
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}

	handler := testPreparedProxyHandler(
		t, route, resource.Service{}, testEffectiveConfig(), testTrafficSplitBinding(t, route),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/split", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf(
			"traffic-split mTLS status = %d, want %d; body=%q",
			response.Code,
			http.StatusNoContent,
			response.Body.String(),
		)
	}
}

func TestTrafficSplitRouteStartsHTTPSActiveProbeForHTTPTarget(t *testing.T) {
	requestSeen := make(chan struct{}, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && r.URL.Path == "/healthz" {
			requestSeen <- struct{}{}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	targetURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse active-probe upstream URL: %v", err)
	}
	port, err := strconv.Atoi(targetURL.Port())
	if err != nil {
		t.Fatalf("parse active-probe upstream port: %v", err)
	}

	route := resource.Route{
		ID:  "traffic-split-active-probe",
		Uri: "/split",
		Plugins: map[string]resource.PluginConfig{
			"traffic-split": map[string]any{
				"rules": []any{map[string]any{
					"weighted_upstreams": []any{map[string]any{
						"weight": 1,
						"upstream": map[string]any{
							"scheme": "http",
							"checks": map[string]any{
								"active": map[string]any{
									"type":                     "https",
									"http_path":                "/healthz",
									"https_verify_certificate": false,
									"healthy":                  map[string]any{"interval": 1, "successes": 1},
								},
							},
							"nodes": []any{map[string]any{
								"host":   targetURL.Hostname(),
								"port":   port,
								"weight": 1,
							}},
						},
					}},
				}},
			},
		},
	}

	_ = testTrafficSplitBinding(t, route)

	select {
	case <-requestSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("traffic-split HTTPS active probe did not reach TLS upstream")
	}
}

func TestTrafficSplitRouteRetriesFromHigherToLowerPriorityNode(t *testing.T) {
	var highCalls atomic.Int32
	high := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		highCalls.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack high-priority response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	t.Cleanup(high.Close)
	var lowCalls atomic.Int32
	low := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lowCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(low.Close)

	highURL, err := url.Parse(high.URL)
	if err != nil {
		t.Fatalf("parse high-priority URL: %v", err)
	}
	highPort, err := strconv.Atoi(highURL.Port())
	if err != nil {
		t.Fatalf("parse high-priority port: %v", err)
	}
	lowURL, err := url.Parse(low.URL)
	if err != nil {
		t.Fatalf("parse low-priority URL: %v", err)
	}
	lowPort, err := strconv.Atoi(lowURL.Port())
	if err != nil {
		t.Fatalf("parse low-priority port: %v", err)
	}

	route := resource.Route{
		ID:  "traffic-split-priority-retry",
		Uri: "/split",
		Plugins: map[string]resource.PluginConfig{
			"traffic-split": map[string]any{
				"rules": []any{map[string]any{
					"weighted_upstreams": []any{map[string]any{
						"weight": 1,
						"upstream": map[string]any{
							"retries": 1,
							"nodes": []any{
								map[string]any{
									"host": "localhost", "port": highPort, "weight": 1, "priority": 10,
								},
								map[string]any{
									"host": lowURL.Hostname(), "port": lowPort, "weight": 1, "priority": 0,
								},
							},
						},
					}},
				}},
			},
		},
		Upstream: resource.Upstream{
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}

	handler := testPreparedProxyHandler(
		t, route, resource.Service{}, testEffectiveConfig(), testTrafficSplitBinding(t, route),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/split", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("traffic-split priority retry status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if got := highCalls.Load(); got != 1 {
		t.Fatalf("high-priority calls = %d, want 1 before fallback", got)
	}
	if got := lowCalls.Load(); got != 1 {
		t.Fatalf("low-priority calls = %d, want 1 after high-priority failure", got)
	}
}

func TestApplyTrafficSplitOverridePassesOriginalHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme:   "http",
		Host:     "127.0.0.1:8080",
		PassHost: "pass",
	})

	if !applyTrafficSplitOverride(req) {
		t.Fatal("applyTrafficSplitOverride() = false, want true")
	}
	if req.URL.Host != "127.0.0.1:8080" {
		t.Fatalf("URL host = %q, want 127.0.0.1:8080", req.URL.Host)
	}
	if req.Host != "route.example.com" {
		t.Fatalf("Host = %q, want route.example.com", req.Host)
	}
}

func TestApplyTrafficSplitOverrideRewritesHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme:       "http",
		Host:         "127.0.0.1:8080",
		PassHost:     "rewrite",
		UpstreamHost: "api.example.com",
	})

	applyTrafficSplitOverride(req)

	if req.Host != "api.example.com" {
		t.Fatalf("Host = %q, want api.example.com", req.Host)
	}
}

func TestApplyTrafficSplitOverrideRetainsRewrittenHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme:         "http",
		Host:           "127.0.0.1:8080",
		PassHost:       "rewrite",
		UpstreamHost:   "split.example",
		HealthReporter: &recordingSplitHealthReporter{},
		HealthTarget:   "http://127.0.0.1:8080",
	})

	applyTrafficSplitOverride(req)

	if req.Host != "split.example" {
		t.Fatalf("Host = %q, want split.example", req.Host)
	}
}

func TestEmptyUpstreamRouteReturnsClassifiedError(t *testing.T) {
	handler, _, err := buildPreparedReverseHandler(
		resource.Route{ID: "empty-upstream"},
		resource.Upstream{},
		nil,
		PreparedUpstreamRuntime{RoundTripper: http.DefaultTransport},
		&testEffectiveConfig().Config,
		nil,
	)
	if err != nil {
		t.Fatalf("buildPreparedReverseHandler() error = %v, want plugin-only route support", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d without a picker panic", response.Code, http.StatusBadGateway)
	}
}

func TestErrorHandlerClassifiesDirectorErrorOnce(t *testing.T) {
	directorErr := errors.New("parse host fail, invalid target")
	request := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	request = withDirectorError(request, directorErr)

	response := httptest.NewRecorder()
	newErrorHandler(&testEffectiveConfig().Config)(response, request, errors.New("http: no Host in request URL"))

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
	}
	if strings.Contains(response.Body.String(), "http: no Host in request URL") {
		t.Fatalf("body = %q, leaks the raw proxy error", response.Body.String())
	}
}

func TestErrorHandlerClassifiesWrappedErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "wrapped upstream EOF",
			err:        fmt.Errorf("read response body: %w", io.EOF),
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "wrapped client cancellation",
			err:        fmt.Errorf("copy response body: %w", context.Canceled),
			wantStatus: StatusClientClosedRequest,
		},
		{
			name:       "wrapped unexpected EOF",
			err:        fmt.Errorf("copy request body: %w", io.ErrUnexpectedEOF),
			wantStatus: StatusClientClosedRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
			response := httptest.NewRecorder()
			newErrorHandler(&testEffectiveConfig().Config)(response, request, test.err)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
