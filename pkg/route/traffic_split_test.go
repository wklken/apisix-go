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

	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestApplyTrafficSplitOverrideUpdatesProxyTarget(t *testing.T) {
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
	if req.Host != "shadow.example.com:9443" {
		t.Fatalf("Host = %q, want shadow.example.com:9443", req.Host)
	}
}

func TestTrafficSplitRouteUsesSelectedUpstreamMTLS(t *testing.T) {
	ensureRouteStore(t)
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

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080", testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(route)
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}

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
	ensureRouteStore(t)
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

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080", testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	if _, err := builder.buildHandlerStrict(route); err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}

	select {
	case <-requestSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("traffic-split HTTPS active probe did not reach TLS upstream")
	}
}

func TestTrafficSplitRouteRetriesFromHigherToLowerPriorityNode(t *testing.T) {
	ensureRouteStore(t)
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

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080", testEffectiveConfig(), testDataEncryptionResolver())
	t.Cleanup(builder.Stop)
	handler, err := builder.buildHandlerStrict(route)
	if err != nil {
		t.Fatalf("buildHandlerStrict() error = %v", err)
	}
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
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	handler, err := builder.buildReverseHandler(resource.Route{}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v, want plugin-only route support", err)
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
