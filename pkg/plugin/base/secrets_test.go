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
	if !strings.Contains(err.Error(), "nested.credentials[0][0]") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want bounded nested path", err)
	}
	if strings.Contains(err.Error(), "$ENV://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationRejectsLowercaseEnvironmentReferenceWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$env://TOKEN"}})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want lowercase environment reference rejected", err)
	}
	if strings.Contains(err.Error(), "$env://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationRejectsMixedCaseEnvironmentReferenceWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$eNv://TOKEN"}})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want mixed-case environment reference rejected", err)
	}
	if strings.Contains(err.Error(), "$eNv://TOKEN") {
		t.Fatalf("MaterializePluginSecrets() error exposed reference: %v", err)
	}
}

func TestSecretMaterializationAcceptsMixedCaseEnvironmentDescriptorWithoutOwner(t *testing.T) {
	err := MaterializePluginSecrets(configOnlyPlugin{config: struct {
		Token string `json:"token"`
	}{Token: "$eNv://TOKEN#sha256:fingerprint"}})
	if err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v, want mixed-case descriptor accepted", err)
	}
}

func TestSecretMaterializationDepthExhaustionFailsClosed(t *testing.T) {
	config := nestedSecretScanValue(32, "literal")
	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil || !strings.Contains(err.Error(), "secret reference scan depth exceeded") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want bounded depth-exhaustion error", err)
	}
	if strings.Contains(err.Error(), "literal") {
		t.Fatalf("MaterializePluginSecrets() error exposed config value: %v", err)
	}
}

func TestSecretMaterializationFindsReferenceAtMaximumInspectableDepth(t *testing.T) {
	config := nestedSecretScanValue(31, "$ENV://TOKEN")
	err := MaterializePluginSecrets(configOnlyPlugin{config: config})
	if err == nil || !strings.Contains(err.Error(), "unowned secret reference") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want reference at maximum inspectable depth rejected", err)
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
	if errors.Is(err, want) {
		t.Fatalf("MaterializePluginSecrets() error = %v, want no original error chain", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("errors.Unwrap(MaterializePluginSecrets()) = %v, want no error chain", unwrapped)
	}
	if strings.Contains(err.Error(), "$secret://vault/token") {
		t.Fatalf("MaterializePluginSecrets() error exposed secret reference: %v", err)
	}
}

func TestSecretMaterializationDoesNotExposeMapKeyInErrorPath(t *testing.T) {
	const sensitiveMapKey = "credential-$secret://vault/token"
	err := MaterializePluginSecrets(configOnlyPlugin{config: map[string]any{
		sensitiveMapKey: "$ENV://TOKEN",
	}})
	if err == nil {
		t.Fatal("MaterializePluginSecrets() error = nil, want unowned secret reference")
	}
	if strings.Contains(err.Error(), sensitiveMapKey) {
		t.Fatalf("MaterializePluginSecrets() error exposed map key: %v", err)
	}
	if !strings.Contains(err.Error(), "[0]") {
		t.Fatalf("MaterializePluginSecrets() error = %v, want indexed map path", err)
	}
}

type configOnlyPlugin struct {
	config any
}

func (p configOnlyPlugin) Config() any {
	return p.config
}

func nestedSecretScanValue(depth int, value any) any {
	for range depth {
		value = []any{value}
	}
	return value
}
