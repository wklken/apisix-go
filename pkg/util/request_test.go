package util

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestSize(t *testing.T) {
	tests := []struct {
		name string
		req  *http.Request
		want int64
	}{
		{name: "declared content length", req: httptest.NewRequest(http.MethodPost, "/", nil), want: 0},
		{name: "unknown content length", req: &http.Request{}, want: 0},
		{name: "chunked request", req: &http.Request{ContentLength: -1}, want: 0},
	}

	tests[0].req.ContentLength = 128
	tests[0].want = 128

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequestSize(tt.req); got != tt.want {
				t.Fatalf("RequestSize() = %d, want %d", got, tt.want)
			}
		})
	}
}
