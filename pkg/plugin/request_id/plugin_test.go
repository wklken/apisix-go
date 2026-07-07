package request_id

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaAcceptsSnowflakeAlgorithm(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{"algorithm": "snowflake"}, p.GetSchema()); err != nil {
		t.Fatalf("snowflake algorithm should validate: %v", err)
	}
}

func TestSnowflakeGeneratesRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "snowflake"})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-Id")
		if requestID == "" {
			t.Fatal("upstream request is missing X-Request-Id")
		}
		if _, err := strconv.ParseUint(requestID, 10, 64); err != nil {
			t.Fatalf("request id = %q, want unsigned decimal snowflake id: %v", requestID, err)
		}
		if got := r.Context().Value("request_id"); got != requestID {
			t.Fatalf("context request_id = %#v, want %q", got, requestID)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/snowflake", nil))

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if got := rr.Header().Get("X-Request-Id"); got == "" {
		t.Fatal("response is missing generated X-Request-Id")
	}
}

func TestHandlerPreservesIncomingRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "snowflake"})
	req := httptest.NewRequest(http.MethodGet, "/request-id", nil)
	req.Header.Set("X-Request-Id", "client-provided")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Request-Id"); got != "client-provided" {
			t.Fatalf("request id = %q, want client-provided", got)
		}
		if got := r.Context().Value("request_id"); got != "client-provided" {
			t.Fatalf("context request_id = %#v, want client-provided", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("X-Request-Id"); got != "client-provided" {
		t.Fatalf("response request id = %q, want client-provided", got)
	}
}

func TestHandlerCanOmitResponseHeader(t *testing.T) {
	include := false
	p := newTestPlugin(t, Config{IncludeInResponse: &include})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Request-Id"); got == "" {
			t.Fatal("upstream request is missing X-Request-Id")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/request-id", nil))

	if got := rr.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("response request id = %q, want empty", got)
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
