package openfunction

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerInvokesOpenFunctionWithBasicAuthorization(t *testing.T) {
	var gotAuthorization string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("X-Function-Result", "openfunction")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("hello from openfunction"))
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL + "/default/function-sample/test",
		Authorization: &Authorization{
			ServiceToken: "test:test",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusCreated)
	}
	if got := rr.Body.String(); got != "hello from openfunction" {
		t.Fatalf("response body = %q, want function body", got)
	}
	if got := rr.Header().Get("X-Function-Result"); got != "openfunction" {
		t.Fatalf("X-Function-Result = %q, want openfunction", got)
	}
	if gotAuthorization != "Basic dGVzdDp0ZXN0" {
		t.Fatalf("Authorization = %q, want Basic dGVzdDp0ZXN0", gotAuthorization)
	}
}
