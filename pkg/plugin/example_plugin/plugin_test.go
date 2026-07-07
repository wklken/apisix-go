package example_plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	public_api.ResetRegistryForTest()
	t.Cleanup(public_api.ResetRegistryForTest)

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerPassesThrough(t *testing.T) {
	p := newTestPlugin(t, Config{I: 1})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if override := traffic_split.GetOverride(r); override != nil {
			t.Fatalf("override = %#v, want nil", override)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

func TestHandlerSetsUpstreamOverrideWhenIPConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{I: 1, IP: "127.0.0.1", Port: 1980})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override := traffic_split.GetOverride(r)
		if override == nil {
			t.Fatal("override = nil, want upstream override")
		}
		if override.Scheme != "http" {
			t.Fatalf("override scheme = %q, want http", override.Scheme)
		}
		if override.Host != "127.0.0.1:1980" {
			t.Fatalf("override host = %q, want 127.0.0.1:1980", override.Host)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

func TestControlAPIHelloReturnsText(t *testing.T) {
	p := newTestPlugin(t, Config{I: 1})
	handler := public_api.Lookup(http.MethodGet, "/v1/plugin/example-plugin/hello")
	if handler == nil {
		t.Fatal("example-plugin control API was not registered")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "world\n" {
		t.Fatalf("body = %q, want world newline", rr.Body.String())
	}
	if p.GetName() != name {
		t.Fatalf("plugin name = %q, want %q", p.GetName(), name)
	}
}

func TestControlAPIHelloReturnsJSON(t *testing.T) {
	newTestPlugin(t, Config{I: 1})
	handler := public_api.Lookup(http.MethodGet, "/v1/plugin/example-plugin/hello")
	if handler == nil {
		t.Fatal("example-plugin control API was not registered")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello?json=1", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON body: %v", err)
	}
	if body["msg"] != "world" {
		t.Fatalf("msg = %q, want world", body["msg"])
	}
}
