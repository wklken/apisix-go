package function_upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParityRedirectIsConsumed(t *testing.T) {
	calls := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Method + " " + r.URL.Path
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/end")
			w.WriteHeader(302)
			_, _ = w.Write([]byte("redirect-body"))
			return
		}
		_, _ = w.Write([]byte("final-body"))
	}))
	defer server.Close()
	p := newTestPlugin(t, Config{FunctionURI: server.URL + "/start"})
	defer p.Stop()
	rr := httptest.NewRecorder()
	p.Handler(nil).ServeHTTP(rr, httptest.NewRequest("POST", "http://gateway/function", strings.NewReader("payload")))
	t.Logf("observed status=%d body=%q", rr.Code, rr.Body.String())
	if rr.Code != 302 {
		t.Errorf("APISIX should preserve upstream 302, got %d", rr.Code)
	}
	if call := <-calls; call != "POST /start" {
		t.Errorf("upstream call = %q", call)
	}
	select {
	case call := <-calls:
		t.Errorf("unexpected redirect follow-up %q", call)
	default:
	}
}
