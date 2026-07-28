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

func TestBuildReverseHandlerAppliesUpstreamPassHost(t *testing.T) {
	receivedHost := make(chan string, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost <- r.Host
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstreamServer.Close()

	target, err := url.Parse(upstreamServer.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	tests := []struct {
		name             string
		passHost         string
		upstreamHost     string
		requestHost      string
		proxyRewriteHost string
		wantHost         string
	}{
		{
			name:        "omitted preserves request host",
			requestHost: "client.example.com",
			wantHost:    "client.example.com",
		},
		{
			name:        "pass preserves request host",
			passHost:    "pass",
			requestHost: "client.example.com",
			wantHost:    "client.example.com",
		},
		{
			name:     "pass falls back to node host",
			passHost: "pass",
			wantHost: target.Host,
		},
		{
			name:        "node uses selected node host",
			passHost:    "node",
			requestHost: "client.example.com",
			wantHost:    target.Host,
		},
		{
			name:         "rewrite uses upstream host",
			passHost:     "rewrite",
			upstreamHost: "upstream.example.com",
			requestHost:  "client.example.com",
			wantHost:     "upstream.example.com",
		},
		{
			name:             "proxy rewrite takes precedence",
			passHost:         "rewrite",
			upstreamHost:     "upstream.example.com",
			requestHost:      "client.example.com",
			proxyRewriteHost: "proxy-rewrite.example.com",
			wantHost:         "proxy-rewrite.example.com",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := (&Builder{}).buildReverseHandler(resource.Route{
				Upstream: resource.Upstream{
					Type:         "roundrobin",
					Scheme:       target.Scheme,
					PassHost:     test.passHost,
					UpstreamHost: test.upstreamHost,
					Nodes: []resource.Node{{
						Host: target.Hostname(), Port: port, Weight: 1,
					}},
				},
			}, resource.Service{})
			if err != nil {
				t.Fatalf("buildReverseHandler() error = %v", err)
			}

			req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/hello", nil)
			req.Host = test.requestHost
			req = apisixctx.WithApisixVars(req, nil)
			if test.proxyRewriteHost != "" {
				rewrite := map[string]any{
					"uri": "", "method": "", "host": test.proxyRewriteHost, "scheme": "",
				}
				req = req.WithContext(context.WithValue(req.Context(), apisixctx.ProxyRewriteKey, rewrite))
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)

			if res.Code != http.StatusNoContent {
				t.Fatalf("response status = %d, want 204", res.Code)
			}
			if got := <-receivedHost; got != test.wantHost {
				t.Fatalf("upstream Host = %q, want %q", got, test.wantHost)
			}
		})
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
