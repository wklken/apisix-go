package route

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		Scheme:       "http",
		Host:         "127.0.0.1:8080",
		PassHost:     "rewrite",
		UpstreamHost: "split.example",
		HealthReporter: &recordingSplitHealthReporter{},
		HealthTarget:   "http://127.0.0.1:8080",
	})

	applyTrafficSplitOverride(req)

	if req.Host != "split.example" {
		t.Fatalf("Host = %q, want split.example", req.Host)
	}
}

func TestEmptyUpstreamRouteReturnsClassifiedError(t *testing.T) {
	builder := &Builder{}
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
	newErrorHandler()(response, request, errors.New("http: no Host in request URL"))

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
			newErrorHandler()(response, request, test.err)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
		})
	}
}
