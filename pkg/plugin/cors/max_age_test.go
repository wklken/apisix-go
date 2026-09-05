package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestPreflightPreservesConfiguredMaxAge(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"omitted", map[string]any{}, "5"},
		{"zero", map[string]any{"max_age": 0}, "0"},
		{"negative", map[string]any{"max_age": -1}, "-1"},
		{"positive", map[string]any{"max_age": 600}, "600"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := util.Parse(tt.config, p.Config()); err != nil {
				t.Fatal(err)
			}
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodOptions, "/", nil)
			req.Header.Set("Origin", "https://client.example")
			req.Header.Set("Access-Control-Request-Method", "GET")
			res := httptest.NewRecorder()
			p.Handler(http.NotFoundHandler()).ServeHTTP(res, req)
			if got := res.Header().Get("Access-Control-Max-Age"); got != tt.want {
				t.Fatalf("max age = %q, want %q", got, tt.want)
			}
		})
	}
}
