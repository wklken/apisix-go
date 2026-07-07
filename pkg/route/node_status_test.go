package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
)

func TestRegisterExtraRoutesAddsNodeStatusWhenEnabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{"node-status"}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/apisix/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["id"] == "" {
		t.Fatalf("id = %v, want non-empty", body["id"])
	}
	status, ok := body["status"].(map[string]any)
	if !ok {
		t.Fatalf("status = %#v, want object", body["status"])
	}
	for _, key := range []string{"active", "accepted", "handled", "total", "reading", "writing", "waiting"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("status[%q] missing in %#v", key, status)
		}
	}
}

func TestRegisterExtraRoutesSkipsNodeStatusWhenDisabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/apisix/status", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestRegisterExtraRoutesAddsServerInfoWhenEnabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{"server-info"}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/server_info", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"etcd_version", "hostname", "id", "version", "boot_time"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("body[%q] missing in %#v", key, body)
		}
	}
	if body["etcd_version"] != "unknown" {
		t.Fatalf("etcd_version = %v, want unknown", body["etcd_version"])
	}
}

func TestRegisterExtraRoutesSkipsServerInfoWhenDisabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/server_info", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNotFound)
	}
}
