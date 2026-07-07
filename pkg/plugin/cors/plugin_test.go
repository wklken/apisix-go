package cors

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

func TestHandlerAllowsRegexOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:        "https://example.com",
		AllowMethods:        http.MethodGet,
		AllowOriginsByRegex: []string{`^https://.+\.test\.com$`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.test.com")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://api.test.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want regex origin", got)
	}
	if got := rr.Code; got != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", got, http.StatusNoContent)
	}
}
