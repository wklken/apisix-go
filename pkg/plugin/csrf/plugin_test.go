package csrf

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
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

func TestHandlerRejectsMissingHeaderWithJSONError(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"error_msg":"no csrf token in headers"}` {
		t.Fatalf("body = %q, want APISIX csrf error JSON", got)
	}
}

func TestCheckCSRFTokenAllowsExpiredTokenWhenExpiresIsZero(t *testing.T) {
	key := "secret"
	token := csrfToken{
		Random:  0.25,
		Expires: 1,
	}
	token.Sign = genSign(token.Random, token.Expires, key)
	body, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}

	if !checkCSRFToken(base64.StdEncoding.EncodeToString(body), key, 0) {
		t.Fatal("checkCSRFToken() = false, want true when expires is zero")
	}
}

func TestPostInitPreservesExplicitZeroExpires(t *testing.T) {
	p := newTestPlugin(t, Config{
		Key:     "secret",
		Expires: int64Ptr(0),
	})

	if got := p.expires(); got != 0 {
		t.Fatalf("expires = %d, want explicit zero preserved", got)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
