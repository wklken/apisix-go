package proxy_rewrite

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParityHeaderSeesOriginalMethod(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{Method: "PATCH", Headers: Headers{Set: HeaderValues{"X-Original-Method": "$request_method"}}},
	)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			t.Fatal(r.Method)
		}
		if got := r.Header.Get("X-Original-Method"); got != "GET" {
			t.Fatalf("got header=%q; APISIX resolves header before set_method and wants GET", got)
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}
