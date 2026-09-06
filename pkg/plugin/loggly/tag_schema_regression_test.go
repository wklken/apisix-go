package loggly

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestTagEqualsPrefixIsRejected(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := util.Validate(map[string]any{
		"customer_token": "tok",
		"tags":           []any{"tag=forbidden"},
	}, p.GetSchema())
	if err == nil {
		t.Fatalf("tag= prefix validation error = %v; want rejection", err)
	}
}
