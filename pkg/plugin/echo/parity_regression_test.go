package echo

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityOfficialRejectsHeadersOnlyEcho(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := util.Validate(map[string]any{
		"headers": map[string]any{"X-Echo": "yes"},
	}, p.GetSchema())
	if err == nil {
		t.Fatal("headers-only echo config accepted; APISIX 3.17 requires before_body, body, or after_body")
	}
}
