package request_validation

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestSchemaRejectRecordsEarlyResponseSource(t *testing.T) {
	p := newTestPlugin(t, Config{
		HeaderSchema: map[string]any{
			"type":     "object",
			"required": []any{"x-required"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("reject reached downstream")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource = %q, want early-stop", got)
	}
}
