package brotli

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
			request.Header.Set("Accept-Encoding", "br")
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

func TestBrotliQualityUsesAPISIXLiteralZeroRule(t *testing.T) {
	for _, tc := range []struct {
		accept     string
		compressed bool
	}{
		{"br;q=0", false},
		{"br;q=0.0", true},
		{"br;q=0.00", true},
		{"*;q=0.0", true},
		{"br;q=0,*;q=0.0", true},
		{"gzip;q=0,identity;q=0", false},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			minimum := 1
			p := newTestPlugin(t, Config{Types: []string{"text/html"}, MinLength: &minimum})
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", tc.accept)
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(strings.Repeat("content ", 30)))
			})).ServeHTTP(response, request)
			if response.Code != http.StatusOK || (response.Header().Get("Content-Encoding") == "br") != tc.compressed {
				t.Fatalf(
					"response = %d encoding=%q; compression want %t",
					response.Code,
					response.Header().Get("Content-Encoding"),
					tc.compressed,
				)
			}
		})
	}
}
