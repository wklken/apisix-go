package base

import (
	"net/http"
	"strings"
	"testing"
)

func TestInvalidateBodyDerivedHeaders(t *testing.T) {
	header := make(http.Header)
	derived := []string{
		"Content-Length",
		"Content-Encoding",
		"Content-Range",
		"Content-MD5",
		"Digest",
		"Content-Digest",
		"Repr-Digest",
		"ETag",
		"Last-Modified",
	}
	for _, field := range derived {
		header[field] = []string{"canonical"}
		header[strings.ToLower(field)] = []string{"lowercase duplicate"}
	}

	header.Set("Content-Type", "application/json")
	header.Set("Content-Language", "en")
	header.Set("Content-Location", "/resource")
	header.Set("Accept-Ranges", "bytes")
	header.Set("Vary", "Accept-Encoding")
	header.Set("Cache-Control", "max-age=60")
	header.Set("X-Extension", "preserve")

	InvalidateBodyDerivedHeaders(header)

	for _, field := range derived {
		for actual := range header {
			if strings.EqualFold(actual, field) {
				t.Errorf("header %q remains after invalidation: %v", actual, header[actual])
			}
		}
	}
	for field, want := range map[string]string{
		"Content-Type":     "application/json",
		"Content-Language": "en",
		"Content-Location": "/resource",
		"Accept-Ranges":    "bytes",
		"Vary":             "Accept-Encoding",
		"Cache-Control":    "max-age=60",
		"X-Extension":      "preserve",
	} {
		if got := header.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

func TestAppendVaryToken(t *testing.T) {
	header := http.Header{
		"Vary": {"Origin, Accept-Encoding", "accept-language, origin"},
	}

	AppendVaryToken(header, "ACCEPT-ENCODING")

	var tokens []string
	for _, value := range header.Values("Vary") {
		for token := range strings.SplitSeq(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	counts := make(map[string]int)
	for _, token := range tokens {
		counts[strings.ToLower(token)]++
	}
	for _, token := range []string{"origin", "accept-encoding", "accept-language"} {
		if counts[token] != 1 {
			t.Errorf("Vary token %q count = %d, want 1; values = %v", token, counts[token], header.Values("Vary"))
		}
	}
}

func TestResponseAllowsBody(t *testing.T) {
	tests := []struct {
		name   string
		method string
		status int
		want   bool
	}{
		{name: "get 200", method: http.MethodGet, status: http.StatusOK, want: true},
		{name: "head", method: http.MethodHead, status: http.StatusOK, want: false},
		{name: "100", method: http.MethodGet, status: 100, want: false},
		{name: "101", method: http.MethodGet, status: http.StatusSwitchingProtocols, want: false},
		{name: "199", method: http.MethodGet, status: 199, want: false},
		{name: "204", method: http.MethodGet, status: http.StatusNoContent, want: false},
		{name: "304", method: http.MethodGet, status: http.StatusNotModified, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResponseAllowsBody(tt.method, tt.status); got != tt.want {
				t.Fatalf("ResponseAllowsBody(%q, %d) = %t, want %t", tt.method, tt.status, got, tt.want)
			}
		})
	}
}
