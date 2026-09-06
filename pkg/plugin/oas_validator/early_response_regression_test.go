package oas_validator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestOASRejectRecordsEarlyResponseSource(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: testSpec()})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/no-such-path", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("OAS reject reached downstream")
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "failed to validate request") {
		t.Fatalf("body = %q, want failed to validate request", rr.Body.String())
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource = %q, want early-stop", got)
	}
}
