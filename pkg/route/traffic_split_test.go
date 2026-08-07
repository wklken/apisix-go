package route

import (
	"errors"
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
	if !strings.Contains(response.Body.String(), directorErr.Error()) {
		t.Fatalf("body = %q, want the classified director error", response.Body.String())
	}
}
