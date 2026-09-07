package loggly

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestTagRejectsNonPrintableBytes(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(
		map[string]any{"customer_token": "tok", "tags": []any{"line\nbreak"}},
		p.GetSchema(),
	); err == nil {
		t.Fatal("tag containing a newline was accepted; APISIX requires printable ASCII")
	}
}
