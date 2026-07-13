package request_id

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaAcceptsUUIDv7AndKSUIDAlgorithms(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, algorithm := range []string{"uuidv7", "ksuid"} {
		if err := util.Validate(map[string]any{"algorithm": algorithm}, p.GetSchema()); err != nil {
			t.Fatalf("%s algorithm should validate: %v", algorithm, err)
		}
	}
}

func TestUUIDv7GeneratesVersionSevenRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuidv7"})
	requestID := generatedRequestID(t, p)
	if len(requestID) != 36 || requestID[14] != '7' {
		t.Fatalf("request id = %q, want UUIDv7 format", requestID)
	}
}

func TestKSUIDGeneratesSortableBase62RequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "ksuid"})
	requestID := generatedRequestID(t, p)
	if len(requestID) != 27 {
		t.Fatalf("request id = %q, want 27-character KSUID", requestID)
	}
	if strings.Trim(requestID, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") != "" {
		t.Fatalf("request id = %q, want base62 characters", requestID)
	}
}

func TestHandlerPreservesIncomingRequestID(t *testing.T) {
	p := newTestPlugin(t, Config{Algorithm: "uuid"})
	req := httptest.NewRequest(http.MethodGet, "/request-id", nil)
	req.Header.Set("X-Request-Id", "client-provided")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Request-Id"); got != "client-provided" {
			t.Fatalf("request id = %q, want client-provided", got)
		}
		if got := r.Context().Value(apisixctx.RequestIDKey); got != "client-provided" {
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

func generatedRequestID(t *testing.T, p *Plugin) string {
	t.Helper()
	var requestID string
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/request-id", nil))
	if requestID == "" {
		t.Fatal("request id is empty")
	}
	return requestID
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
