package prometheus

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
)

func TestHandlerPassesThrough(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPostInitRegistersMetricsPublicAPI(t *testing.T) {
	public_api.ResetRegistryForTest()
	t.Cleanup(public_api.ResetRegistryForTest)

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	handler := public_api.Lookup(http.MethodGet, MetricsURI)
	if handler == nil {
		t.Fatalf("public API handler for %s was not registered", MetricsURI)
	}

	metricName := fmt.Sprintf("apisix_go_test_prometheus_public_api_value_%d", time.Now().UnixNano())
	gauge := promclient.NewGauge(promclient.GaugeOpts{
		Name: metricName,
		Help: "test metric for prometheus public api registration",
	})
	if err := promclient.Register(gauge); err != nil {
		t.Fatalf("register test metric: %v", err)
	}
	t.Cleanup(func() {
		promclient.Unregister(gauge)
	})
	gauge.Set(7)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetricsURI, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want prometheus text exposition", got)
	}
	if !strings.Contains(rec.Body.String(), metricName) {
		t.Fatalf("metrics response does not include test metric %q", metricName)
	}
}
