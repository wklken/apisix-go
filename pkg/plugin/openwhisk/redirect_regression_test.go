package openwhisk

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenWhiskDoesNotFollowActionRedirect(t *testing.T) {
	calls := 0
	action := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/second" {
			_, _ = w.Write([]byte(`{"statusCode":200,"body":"followed"}`))
			return
		}
		w.Header().Set("Location", "/second")
		w.WriteHeader(302)
		_, _ = w.Write([]byte("redirect"))
	}))
	defer action.Close()
	p := newTestPlugin(t, Config{APIHost: action.URL, ServiceToken: "token", Namespace: "guest", Action: "hello"})
	response := httptest.NewRecorder()
	p.Handler(nil).ServeHTTP(response, httptest.NewRequest("POST", "/invoke", nil))
	if calls != 1 || response.Code != 503 {
		t.Fatalf(
			"origin calls=%d status=%d body=%q, want one call and non-JSON parse 503",
			calls,
			response.Code,
			response.Body.String(),
		)
	}
}
