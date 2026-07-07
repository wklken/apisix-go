package graphql_limit_count

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestGraphQLDepthCountsNestedSelections(t *testing.T) {
	depth, err := queryDepth(`query { foo { bar { baz { id } } } }`)
	if err != nil {
		t.Fatalf("queryDepth() error = %v", err)
	}
	if depth != 4 {
		t.Fatalf("depth = %d, want 4", depth)
	}

	depth, err = queryDepth(`query { foo { ...Fields } } fragment Fields on Foo { bar { id } }`)
	if err != nil {
		t.Fatalf("queryDepth() with fragment error = %v", err)
	}
	if depth != 3 {
		t.Fatalf("fragment depth = %d, want 3", depth)
	}
}

func TestHandlerLimitsJSONGraphQLByDepthCost(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:                5,
		TimeWindow:           60,
		Key:                  "remote_addr",
		RejectedCode:         http.StatusTooManyRequests,
		ShowLimitQuotaHeader: boolPtr(true),
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/graphql",
		strings.NewReader(`{"query":"query { foo { bar { baz { id } } } }"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "5" {
		t.Fatalf("X-RateLimit-Limit = %q, want 5", got)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { bar } }"}`))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.RemoteAddr = "192.0.2.10:1234"
	rr = httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when quota is exhausted")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second response code = %d, want 429", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("rejected X-RateLimit-Remaining = %q, want 0", got)
	}
}

func TestHandlerAcceptsApplicationGraphQLBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      3,
		TimeWindow: 60,
		Key:        "remote_addr",
	})

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`query { foo { id } }`))
	req.Header.Set("Content-Type", "application/graphql")
	req.RemoteAddr = "192.0.2.11:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
	if got := rr.Header().Get("X-RateLimit-Remaining"); got != "1" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 1", got)
	}
}

func TestHandlerRejectsInvalidGraphQLRequests(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      3,
		TimeWindow: 60,
		Key:        "remote_addr",
	})

	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "get method", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed},
		{
			name:        "unsupported content type",
			method:      http.MethodPost,
			contentType: "text/plain",
			body:        `query { foo }`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "missing query field",
			method:      http.MethodPost,
			contentType: "application/json",
			body:        `{"variables":{}}`,
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid query",
			method:      http.MethodPost,
			contentType: "application/graphql",
			body:        `query { foo { `,
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/graphql", strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("response code = %d, want %d", rr.Code, tt.wantStatus)
			}
		})
	}
}

func TestWindowResetsAfterTimeWindow(t *testing.T) {
	p := newTestPlugin(t, Config{
		Count:      2,
		TimeWindow: 1,
		Key:        "remote_addr",
	})
	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p.now = func() time.Time { return base }

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.12:1234"
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("first response code = %d, want 204", rr.Code)
	}

	p.now = func() time.Time { return base.Add(2 * time.Second) }
	req = httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ foo { id } }"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.12:1234"
	rr = httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second response code = %d, want 204 after reset", rr.Code)
	}
}

func boolPtr(value bool) *bool {
	return &value
}
