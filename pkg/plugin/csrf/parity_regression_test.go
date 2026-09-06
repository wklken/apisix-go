package csrf

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestParityEmptyKeyAdmittedAndUsable(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(map[string]any{"key": ""}, p.GetSchema()); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newCSRFScopedSecretHarness(t, 9, "csrf-empty", "", "")
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if err := p.useKey(func(key string) error {
		token, err := genCSRFToken(key, rand.Reader)
		if err != nil {
			return err
		}
		if !checkCSRFToken(token, key, p.expires()) {
			t.Error("empty-key token fails validation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
