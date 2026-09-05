package gzip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompressionPreservesUpstreamWhenNoCodingAvailable(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0, identity;q=0")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("upstream response"))
	})).ServeHTTP(res, req)
	if res.Code != 200 || res.Body.String() != "upstream response" {
		t.Fatalf("status=%d body=%q; APISIX preserves upstream response", res.Code, res.Body.String())
	}
}
