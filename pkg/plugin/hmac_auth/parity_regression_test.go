package hmac_auth

import (
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityRealmAdmission(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for _, realm := range []string{"", "bad\"realm", "bad\\realm", "bad\nrealm", strings.Repeat("a", 129)} {
		if err := util.Validate(map[string]any{"realm": realm}, p.GetSchema()); err == nil {
			t.Errorf("invalid realm %q admitted", realm)
		}
	}
	for _, realm := range []string{"normal realm", "!#[]~", strings.Repeat("a", 128)} {
		if err := util.Validate(map[string]any{"realm": realm}, p.GetSchema()); err != nil {
			t.Errorf("valid realm %q rejected: %v", realm, err)
		}
	}
}
