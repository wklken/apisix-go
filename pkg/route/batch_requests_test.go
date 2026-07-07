package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
)

func TestRegisterExtraRoutesAddsBatchRequestsWhenEnabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{"batch-requests"}}

	mux := chi.NewRouter()
	mux.Get("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Global", r.Header.Get("X-Global"))
		w.Header().Set("X-Seen-Item", r.Header.Get("X-Item"))
		_, _ = w.Write([]byte("hello " + r.URL.Query().Get("name") + " " + r.URL.Query().Get("token")))
	})
	mux.Post("/submit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{
		"query": {"token": "global"},
		"headers": {"X-Global": "yes"},
		"pipeline": [
			{"method": "GET", "path": "/hello", "query": {"name": "alice"}, "headers": {"X-Item": "one"}},
			{"method": "POST", "path": "/submit", "body": "payload"}
		]
	}`))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()

	mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%s", res.Code, res.Body.String())
	}
	var body []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body) != 2 {
		t.Fatalf("response len = %d, want 2: %#v", len(body), body)
	}
	if body[0]["status"] != float64(http.StatusOK) {
		t.Fatalf("first status = %v, want 200", body[0]["status"])
	}
	if body[0]["body"] != "hello alice global" {
		t.Fatalf("first body = %q, want merged query response", body[0]["body"])
	}
	headers, ok := body[0]["headers"].(map[string]any)
	if !ok {
		t.Fatalf("first headers = %#v, want object", body[0]["headers"])
	}
	if headers["X-Seen-Global"] != "yes" {
		t.Fatalf("X-Seen-Global = %v, want yes", headers["X-Seen-Global"])
	}
	if headers["X-Seen-Item"] != "one" {
		t.Fatalf("X-Seen-Item = %v, want one", headers["X-Seen-Item"])
	}
	if body[1]["status"] != float64(http.StatusCreated) {
		t.Fatalf("second status = %v, want 201", body[1]["status"])
	}
	if body[1]["body"] != "created" {
		t.Fatalf("second body = %q, want created", body[1]["body"])
	}
}

func TestRegisterExtraRoutesSkipsBatchRequestsWhenDisabled(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{"pipeline":[]}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusNotFound {
		t.Fatalf("response code = %d, want 404", res.Code)
	}
}

func TestBatchRequestsRejectsInvalidBody(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() {
		config.GlobalConfig = oldConfig
	})
	config.GlobalConfig = &config.Config{Plugins: []string{"batch-requests"}}

	mux := chi.NewRouter()
	registerExtraRoutes(mux)

	req := httptest.NewRequest(http.MethodPost, "/apisix/batch-requests", strings.NewReader(`{"pipeline":[]}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", res.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if !strings.Contains(body["error_msg"], "pipeline") {
		t.Fatalf("error_msg = %q, want pipeline validation error", body["error_msg"])
	}
}
