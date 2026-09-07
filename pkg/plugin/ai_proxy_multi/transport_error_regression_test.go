package ai_proxy_multi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestRegressionTransportErrorRendersDefaultAPISIXPage(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			Instances: []Instance{
				{
					Name:     "one",
					Provider: "openai-compatible",
					Weight:   1,
					Auth:     Auth{Header: map[string]string{"Authorization": "Bearer test-token"}},
					Options:  map[string]any{"model": "gpt-4"},
					Override: Override{Endpoint: "http://192.0.2.10/v1/chat/completions"},
				},
			},
		},
	)
	p.client.Transport = multiStreamRoundTripFunc(
		func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded },
	)
	req := apisixctx.WithRequestVars(
		httptest.NewRequest("POST", "/anything", strings.NewReader(`{"messages":[{"role":"user","content":"ping"}]}`)),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != 504 {
		t.Fatalf("status=%d", rr.Code)
	}
	const wantBody = "<html>\r\n<head><title>504 Gateway Time-out</title></head>\r\n" +
		"<body>\r\n<center><h1>504 Gateway Time-out</h1></center>\r\n" +
		"<hr><center>openresty</center>\r\n" +
		"<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p>" +
		"</body>\r\n</html>\r\n"
	if rr.Body.String() != wantBody {
		t.Fatalf("body=%q, want default APISIX page", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
}
