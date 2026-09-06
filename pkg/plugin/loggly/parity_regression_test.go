package loggly

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityLogglyMetadataRejectsUnknownProtocol(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(map[string]any{"protocol": "custom"}, p.GetMetadataSchema()); err == nil {
		t.Fatal("metadata protocol custom accepted; APISIX 3.17 enum permits only syslog/http/https")
	}
}
