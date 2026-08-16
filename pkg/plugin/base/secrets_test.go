package base

import (
	"errors"
	"strings"
	"testing"
)

type secretMaterializationTestPlugin struct {
	config           any
	materialize      func()
	materializeErr   error
	materializeCalls int
}

func (p *secretMaterializationTestPlugin) Config() any {
	return p.config
}

func (p *secretMaterializationTestPlugin) MaterializeSecrets() error {
	p.materializeCalls++
	if p.materialize != nil {
		p.materialize()
	}
	return p.materializeErr
}

type secretMaterializationTestConfig struct {
	Token  string `json:"token"`
	Nested struct {
		Credentials []map[string]string `json:"credentials"`
	} `json:"nested"`
}

func TestSecretMaterializationAllowsLiteralConfigWithoutOwner(t *testing.T) {
	p := &secretMaterializationTestPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "literal-token"}}

	if err := MaterializePluginSecrets(configOnlyPlugin{config: p.config}); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v, want literal config accepted", err)
	}
}

func TestSecretMaterializationRejectsUnownedReferenceAtNestedPath(t *testing.T) {
	config := secretMaterializationTestConfig{}
	config.Nested.Credentials = []map[string]string{{"token": "$ENV://TOKEN"}}

	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil {
		t.Fatal("MaterializePluginSecrets() error = nil, want unowned secret reference")
	}
	if !strings.Contains(err.Error(), "nested.credentials[0].token") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want bounded nested path", err)
	}
	if strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationInvokesOwnerOnceAndAcceptsDescriptor(t *testing.T) {
	config := &secretMaterializationTestConfig{Token: "$ENV://TOKEN"}
	p := &secretMaterializationTestPlugin{config: config}
	p.materialize = func() {
		config.Token = "$ENV://TOKEN#sha256:fingerprint"
	}

	if err := MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if p.materializeCalls != 1 {
		t.Fatalf("MaterializeSecrets() calls = %d, want 1", p.materializeCalls)
	}
}

func TestSecretMaterializationRedactsOwnerError(t *testing.T) {
	want := errors.New("failed to resolve $secret://vault/token")
	p := &secretMaterializationTestPlugin{materializeErr: want}

	err := MaterializePluginSecrets(p)
	if !errors.Is(err, want) {
		t.Fatalf("MaterializePluginSecrets() error = %v, want wrapped materializer error", err)
	}
	if strings.Contains(err.Error(), "$secret://vault/token") {
		t.Fatalf("MaterializePluginSecrets() error exposed secret reference: %v", err)
	}
}

type configOnlyPlugin struct {
	config any
}

func (p configOnlyPlugin) Config() any {
	return p.config
}
