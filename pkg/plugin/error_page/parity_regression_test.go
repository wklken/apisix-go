package error_page

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestParityExplicitEmptyBodyMustSkipPage(t *testing.T) {
	p := newTestPluginWithMetadata(t, []byte(`{"enable":true,"error_404":{"body":"","content_type":"text/plain"}}`))
	r := apisixctx.WithRequestVars(httptest.NewRequest("GET", "http://example.test/missing", nil))
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceEarlyStop)
	state := base.ResponseState{
		Status: 404,
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(`{"message":"not found"}`),
	}
	if err := p.RunBufferedBodyFilter(r, &state); err != nil {
		t.Fatal(err)
	}
	if string(state.Body) != `{"message":"not found"}` {
		t.Fatalf("explicit empty metadata body unexpectedly replaced existing body: %q", state.Body)
	}
}

func TestOmittedErrorPageDoesNotReplaceResponse(t *testing.T) {
	for _, document := range []string{`{"enable":true}`, `{"enable":true,"error_404":{}}`} {
		for _, status := range []int{404, 500, 502, 503} {
			if status == 404 && document != `{"enable":true}` {
				continue
			}
			t.Run(document+http.StatusText(status), func(t *testing.T) {
				p := newTestPluginWithMetadata(t, []byte(document))
				response := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(status)
					_, _ = w.Write([]byte("original"))
				})
				if response.Code != status || response.Body.String() != "original" ||
					response.Header().Get("Content-Type") != "text/plain" {
					t.Fatalf(
						"unconfigured response = %d %q %q",
						response.Code,
						response.Body.String(),
						response.Header().Get("Content-Type"),
					)
				}
				request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
				apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
				state := base.ResponseState{
					Status: status,
					Header: http.Header{"Content-Type": {"text/plain"}},
					Body:   []byte("original"),
				}
				if err := p.RunBufferedBodyFilter(request, &state); err != nil {
					t.Fatal(err)
				}
				if string(state.Body) != "original" || state.Header.Get("Content-Type") != "text/plain" {
					t.Fatalf("unconfigured buffered response = %+v", state)
				}
			})
		}
	}
}
