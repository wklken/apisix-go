package redirect

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParityRegexCaptureWithTextSuffix(t *testing.T) {
	p := newTestPlugin(t, Config{RegexUri: []string{"^/([a-z]+)$", "/new/$1_suffix"}})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rr, httptest.NewRequest("GET", "/item", nil))
	if got := rr.Header().Get("Location"); got != "/new/item_suffix" {
		t.Fatalf("got Location=%q; numeric capture plus literal suffix wants /new/item_suffix", got)
	}
}
