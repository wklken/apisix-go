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
