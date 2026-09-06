package fault_injection

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityOfficialAcceptsUnboundedFaultStatus(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := util.Validate(map[string]any{
		"abort": map[string]any{"http_status": 600},
	}, p.GetSchema())
	if err != nil {
		t.Fatalf("abort.http_status=600 rejected; APISIX 3.17 schema has minimum 200 and no maximum: %v", err)
	}
}
