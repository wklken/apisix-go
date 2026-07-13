package dubbo_proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestHandlerStoresDubboProxyMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{
		ServiceName:    "org.apache.dubbo.sample.DemoService",
		ServiceVersion: "0.0.0",
		Method:         "sayHello",
	})

	req := httptest.NewRequest(http.MethodPost, "/ignored", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Enabled(r) {
			t.Fatal("Enabled() = false, want true")
		}
		if got := ServiceName(r); got != "org.apache.dubbo.sample.DemoService" {
			t.Fatalf("ServiceName() = %q, want configured service", got)
		}
		if got := ServiceVersion(r); got != "0.0.0" {
			t.Fatalf("ServiceVersion() = %q, want 0.0.0", got)
		}
		if got := Method(r); got != "sayHello" {
			t.Fatalf("Method() = %q, want sayHello", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDefaultsMethodFromURI(t *testing.T) {
	p := newTestPlugin(t, Config{
		ServiceName:    "org.apache.dubbo.sample.DemoService",
		ServiceVersion: "1.2.3",
	})

	req := httptest.NewRequest(http.MethodGet, "/sayHello", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := Method(r); got != "sayHello" {
			t.Fatalf("Method() = %q, want URI without leading slash", got)
		}
		cfg, ok := GetConfig(r)
		if !ok || cfg.Method != "sayHello" {
			t.Fatalf("GetConfig().Method = %q (ok=%t), want sayHello", cfg.Method, ok)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}
