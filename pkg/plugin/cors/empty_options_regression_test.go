package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestExplicitEmptyOptionsRemainValidWithCredentials(t *testing.T) {
	for _, tc := range []struct{ name, methods, headers string }{
		{"methods", "", "X-Token"},
		{"headers", "GET", ""},
		{"both", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(
				map[string]any{
					"allow_origins":    "http://test.com",
					"allow_methods":    tc.methods,
					"allow_headers":    tc.headers,
					"expose_headers":   "",
					"allow_credential": true,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			var cfg Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			p := newTestPlugin(t, cfg)
			req := httptest.NewRequest(http.MethodGet, "/hello", nil)
			req.Header.Set("Origin", "http://test.com")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).
				ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d", rr.Code)
			}
			for header, want := range map[string]string{"Access-Control-Allow-Methods": tc.methods, "Access-Control-Allow-Headers": tc.headers, "Access-Control-Allow-Credentials": "true"} {
				if got := rr.Header().Get(header); got != want {
					t.Fatalf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}
