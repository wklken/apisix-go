package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestApplyProxyRewriteURIUpdatesPathAndQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/old?keep=0", nil)

	applyProxyRewriteURI(req, "/private/v1?token=redacted")

	if req.URL.Path != "/private/v1" {
		t.Fatalf("path = %q, want /private/v1", req.URL.Path)
	}
	if req.URL.RawQuery != "token=redacted" {
		t.Fatalf("raw query = %q, want token=redacted", req.URL.RawQuery)
	}
}

func TestApplyProxyRewriteURIPreservesEscapedPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/old", nil)

	applyProxyRewriteURI(req, "/private/%2Fraw?token=redacted")

	if req.URL.Path != "/private//raw" {
		t.Fatalf("path = %q, want decoded path /private//raw", req.URL.Path)
	}
	if req.URL.RawPath != "/private/%2Fraw" {
		t.Fatalf("raw path = %q, want /private/%%2Fraw", req.URL.RawPath)
	}
	if got := req.URL.EscapedPath(); got != "/private/%2Fraw" {
		t.Fatalf("escaped path = %q, want /private/%%2Fraw", got)
	}
	if got := req.URL.RequestURI(); got != "/private/%2Fraw?token=redacted" {
		t.Fatalf("request URI = %q, want encoded path and query", got)
	}
}

func TestBuildReverseHandlerRewritesHostWithoutChangingTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.example.com" {
			t.Errorf("upstream Host = %q, want api.example.com", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	handler, err := (&Builder{}).buildReverseHandler(resource.Route{
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: target.Scheme,
			Nodes: []resource.Node{{
				Host: target.Hostname(), Port: port, Weight: 1,
			}},
		},
	}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/hello", nil)
	req = apisixctx.WithApisixVars(req, nil)
	rewrite := map[string]any{
		"uri": "", "method": "", "host": "api.example.com", "scheme": "",
	}
	req = req.WithContext(context.WithValue(req.Context(), apisixctx.ProxyRewriteKey, rewrite))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want 204", res.Code)
	}
	if got := apisixctx.GetApisixVar(req, "$balancer_ip"); got != target.Hostname() {
		t.Fatalf("$balancer_ip = %v, want %s", got, target.Hostname())
	}
	if got := apisixctx.GetApisixVar(req, "$balancer_port"); got != target.Port() {
		t.Fatalf("$balancer_port = %v, want %s", got, target.Port())
	}
}

func TestBuildReverseHandlerKeepsTrafficSplitTargetWithRewrittenHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "api.example.com" {
			t.Errorf("upstream Host = %q, want api.example.com", r.Host)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	handler, err := (&Builder{}).buildReverseHandler(resource.Route{
		Upstream: resource.Upstream{
			Type:   "roundrobin",
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/hello", nil)
	req = apisixctx.WithApisixVars(req, nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{Scheme: target.Scheme, Host: target.Host})
	rewrite := map[string]any{
		"uri": "", "method": "", "host": "api.example.com", "scheme": "",
	}
	req = req.WithContext(context.WithValue(req.Context(), apisixctx.ProxyRewriteKey, rewrite))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want 204", res.Code)
	}
	if got := apisixctx.GetApisixVar(req, "$balancer_ip"); got != target.Hostname() {
		t.Fatalf("$balancer_ip = %v, want %s", got, target.Hostname())
	}
}
