package gzip

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressionContentTypeUsesExactConfiguredSpelling(t *testing.T) {
	for _, tc := range []struct {
		configured, actual string
		compressed         bool
	}{
		{"text/html", "Text/HTML", false},
		{"Text/HTML", "Text/HTML", true},
		{"Text/HTML", "text/html", false},
		{"text/html", "text/html; charset=utf-8", true},
		{"text/html", "text/html ; charset=utf-8", false},
	} {
		t.Run(tc.configured+"/"+tc.actual, func(t *testing.T) {
			minimum := 1
			p := newTestPlugin(t, Config{Types: []string{tc.configured}, MinLength: &minimum})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", "gzip")
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.actual)
				_, _ = w.Write([]byte(strings.Repeat("content ", 30)))
			})).ServeHTTP(response, request)
			if got := response.Header().Get("Content-Encoding") != ""; got != tc.compressed {
				t.Fatalf(
					"Content-Encoding = %q; compressed want %t",
					response.Header().Get("Content-Encoding"),
					tc.compressed,
				)
			}
		})
	}
}
