package secret

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

type referenceResolverFunc func(context.Context, string) (string, error)

func (resolve referenceResolverFunc) Resolve(ctx context.Context, raw string) (string, error) {
	return resolve(ctx, raw)
}

type passthroughResolver struct{}

func (passthroughResolver) Resolve(_ context.Context, raw string) (string, error) {
	return raw, nil
}

type captureScopedResolver struct {
	scope     Scope
	raw       string
	plaintext string
	err       error
}

func (resolver *captureScopedResolver) ResolveScoped(_ context.Context, scope Scope, raw string) (string, error) {
	resolver.scope = scope
	resolver.raw = raw
	return resolver.plaintext, resolver.err
}

func ownedScope() Scope {
	return Scope{
		Generation: 9,
		Plugin:     "http-logger",
		Resource:   generation.ResourceKey{Kind: "routes", ID: "r1"},
		Field:      "auth_header",
	}
}

func valuePlaintext(t *testing.T, value Value) string {
	t.Helper()
	var plaintext string
	if err := value.Use(func(value string) error {
		plaintext = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func TestMaterializerRequiresOwnedScopeAndNeverLeaksPlaintext(t *testing.T) {
	const plaintext = "credential-value"
	refs := referenceResolverFunc(func(context.Context, string) (string, error) {
		return plaintext, nil
	})
	materializer := NewMaterializer(data_encryption.NewService(false, nil), refs)

	_, err := materializer.Materialize(context.Background(), Scope{}, "$ENV://TOKEN")
	if err == nil || strings.Contains(err.Error(), plaintext) {
		t.Fatalf("Materialize() error = %v, want redacted scope error", err)
	}

	value, err := materializer.Materialize(context.Background(), ownedScope(), "$ENV://TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := value.Use(func(value string) error {
		got = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got != plaintext {
		t.Fatalf("materialized plaintext = %q, want %q", got, plaintext)
	}
	if gotDigest, wantDigest := value.Digest(), sha256.Sum256([]byte(plaintext)); gotDigest != wantDigest {
		t.Fatalf("materialized digest = %x, want %x", gotDigest, wantDigest)
	}
}

func TestGenerationCapabilityRejectsCrossGenerationScope(t *testing.T) {
	base := NewMaterializer(data_encryption.NewService(false, nil), passthroughResolver{})
	capability, err := NewGenerationCapability(base, 9)
	if err != nil {
		t.Fatal(err)
	}
	scope := ownedScope()
	scope.Generation = 10
	_, err = capability.Materialize(context.Background(), scope, "value")
	if !errors.Is(err, ErrCapabilityScopeMismatch) {
		t.Fatalf("Materialize() error = %v, want ErrCapabilityScopeMismatch", err)
	}
}

func TestScopedMaterializerPassesOwnedScopeAndRedactsResolverError(t *testing.T) {
	const plaintext = "credential-value"
	scope := ownedScope()
	resolver := &captureScopedResolver{plaintext: plaintext}
	materializer := NewScopedMaterializer(resolver)

	value, err := materializer.Materialize(context.Background(), scope, "$secret://logger/token")
	if err != nil {
		t.Fatal(err)
	}
	if resolver.scope != scope || resolver.raw != "$secret://logger/token" {
		t.Fatalf("resolver input = %#v/%q, want %#v/%q", resolver.scope, resolver.raw, scope, "$secret://logger/token")
	}
	if got, want := value.Digest(), sha256.Sum256([]byte(plaintext)); got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}

	resolver.err = errors.New("backend included " + plaintext)
	_, err = materializer.Materialize(context.Background(), scope, "$secret://logger/token")
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), plaintext) {
		t.Fatalf("Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
	}
}

func TestMaterializerUsesCanonicalAtRestContextAcrossGenerations(t *testing.T) {
	const key = "0123456789abcdef"
	service := data_encryption.NewService(true, []string{key})
	configs := map[string]any{
		"error-log-logger": map[string]any{
			"clickhouse": map[string]any{"password": "credential-value"},
		},
	}
	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	ciphertext := configs["error-log-logger"].(map[string]any)["clickhouse"].(map[string]any)["password"].(string)
	materializer := NewMaterializer(service, passthroughResolver{})

	baseScope := Scope{
		Plugin:   "error-log-logger",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		Field:    "clickhouse.password",
	}
	for _, generationNumber := range []uint64{9, 10} {
		capability, err := NewGenerationCapability(materializer, generationNumber)
		if err != nil {
			t.Fatal(err)
		}
		scope := baseScope
		scope.Generation = generationNumber
		value, err := capability.Materialize(context.Background(), scope, ciphertext)
		if err != nil {
			t.Fatalf("generation %d Materialize() error = %v", generationNumber, err)
		}
		var got string
		if err := value.Use(func(value string) error { got = value; return nil }); err != nil {
			t.Fatal(err)
		}
		if got != "credential-value" {
			t.Fatalf("generation %d plaintext = %q, want credential-value", generationNumber, got)
		}
	}

	wrongField := baseScope
	wrongField.Generation = 9
	wrongField.Field = "kafka.brokers.*.sasl_config.password"
	_, err := materializer.Materialize(context.Background(), wrongField, ciphertext)
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), ciphertext) {
		t.Fatalf("wrong-field Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
	}
}

func TestMaterializerPreservesStrictAndOptionalPluginFieldSemantics(t *testing.T) {
	const key = "0123456789abcdef"
	service := data_encryption.NewService(true, []string{key})
	configs := map[string]any{
		"http-logger": map[string]any{"auth_header": "strict-secret"},
		"key-auth":    map[string]any{"key": "optional-secret"},
	}
	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	strictCiphertext := configs["http-logger"].(map[string]any)["auth_header"].(string)
	optionalCiphertext := configs["key-auth"].(map[string]any)["key"].(string)
	materializer := NewMaterializer(service, passthroughResolver{})
	strictScope := Scope{
		Generation: 9, Plugin: "http-logger",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"}, Field: "auth_header",
	}
	optionalScope := Scope{
		Generation: 9, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "r1"}, Field: "key",
	}

	tests := []struct {
		name    string
		scope   Scope
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "strict bare literal rejected", scope: strictScope,
			raw: "strict-secret", wantErr: true,
		},
		{
			name: "strict ciphertext resolved", scope: strictScope,
			raw: strictCiphertext, want: "strict-secret",
		},
		{
			name: "strict invalid ciphertext rejected", scope: strictScope,
			raw: "$encrypted://strict-secret", wantErr: true,
		},
		{
			name: "optional bare literal preserved", scope: optionalScope,
			raw: "optional-secret", want: "optional-secret",
		},
		{
			name: "optional ciphertext resolved", scope: optionalScope,
			raw: optionalCiphertext, want: "optional-secret",
		},
		{
			name: "optional invalid ciphertext preserved", scope: optionalScope,
			raw: "$encrypted://optional-secret", want: "$encrypted://optional-secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, err := materializer.Materialize(context.Background(), test.scope, test.raw)
			if test.wantErr {
				if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), test.raw) {
					t.Fatalf("Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := valuePlaintext(t, value); got != test.want {
				t.Fatalf("plaintext = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMaterializerUsesMetadataStrictRegistryAndCanonicalPaths(t *testing.T) {
	const key = "0123456789abcdef"
	service := data_encryption.NewService(true, []string{key})
	strictMetadata := map[string]any{"clickhouse": map[string]any{"password": "strict-secret"}}
	if err := service.EncryptPluginMetadata("error-log-logger", strictMetadata); err != nil {
		t.Fatal(err)
	}
	strictCiphertext := strictMetadata["clickhouse"].(map[string]any)["password"].(string)
	optionalMetadata := map[string]any{"master_apikey": "optional-secret"}
	if err := service.EncryptPluginMetadata("azure-functions", optionalMetadata); err != nil {
		t.Fatal(err)
	}
	optionalCiphertext := optionalMetadata["master_apikey"].(string)
	materializer := NewMaterializer(service, passthroughResolver{})
	strictScope := Scope{
		Generation: 9, Plugin: "error-log-logger",
		Resource: generation.ResourceKey{Kind: "plugin_metadata", ID: "error-log-logger"},
		Field:    "clickhouse.password",
	}
	optionalScope := Scope{
		Generation: 9, Plugin: "azure-functions",
		Resource: generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"}, Field: "master_apikey",
	}

	for _, test := range []struct {
		name    string
		scope   Scope
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "strict metadata bare literal rejected", scope: strictScope,
			raw: "strict-secret", wantErr: true,
		},
		{
			name: "strict metadata ciphertext resolved", scope: strictScope,
			raw: strictCiphertext, want: "strict-secret",
		},
		{
			name: "optional metadata bare literal preserved", scope: optionalScope,
			raw: "optional-secret", want: "optional-secret",
		},
		{
			name: "optional metadata ciphertext resolved", scope: optionalScope,
			raw: optionalCiphertext, want: "optional-secret",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := materializer.Materialize(context.Background(), test.scope, test.raw)
			if test.wantErr {
				if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), test.raw) {
					t.Fatalf("Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := valuePlaintext(t, value); got != test.want {
				t.Fatalf("plaintext = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMaterializerResolvesReferencesBeforeOrAfterAtRestDecryption(t *testing.T) {
	const key = "0123456789abcdef"
	service := data_encryption.NewService(true, []string{key})
	scope := Scope{
		Generation: 9,
		Plugin:     "key-auth",
		Resource:   generation.ResourceKey{Kind: "routes", ID: "r1"},
		Field:      "key",
	}
	configs := map[string]any{"key-auth": map[string]any{"key": "$ENV://TOKEN"}}
	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	encryptedReference := configs["key-auth"].(map[string]any)["key"].(string)

	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "bare reference", raw: "$ENV://TOKEN"},
		{name: "encrypted reference", raw: encryptedReference},
	} {
		t.Run(test.name, func(t *testing.T) {
			materializer := NewMaterializer(service, referenceResolverFunc(
				func(_ context.Context, raw string) (string, error) {
					if raw != "$ENV://TOKEN" {
						t.Fatalf("Resolve() raw = %q, want $ENV://TOKEN", raw)
					}
					return "credential-value", nil
				},
			))
			value, err := materializer.Materialize(context.Background(), scope, test.raw)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			if err := value.Use(func(value string) error { got = value; return nil }); err != nil {
				t.Fatal(err)
			}
			if got != "credential-value" {
				t.Fatalf("plaintext = %q, want credential-value", got)
			}
		})
	}

	literalConfigs := map[string]any{"key-auth": map[string]any{"key": "literal-value"}}
	if err := service.EncryptPluginConfigs(literalConfigs); err != nil {
		t.Fatal(err)
	}
	encryptedLiteral := literalConfigs["key-auth"].(map[string]any)["key"].(string)
	materializer := NewMaterializer(service, referenceResolverFunc(func(context.Context, string) (string, error) {
		t.Fatal("literal must not be sent to the reference resolver")
		return "", nil
	}))
	value, err := materializer.Materialize(context.Background(), scope, encryptedLiteral)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := value.Use(func(value string) error { got = value; return nil }); err != nil {
		t.Fatal(err)
	}
	if got != "literal-value" {
		t.Fatalf("plaintext = %q, want literal-value", got)
	}
}

func TestMaterializerRejectsInvalidCapabilityAndUse(t *testing.T) {
	materializer := NewMaterializer(data_encryption.NewService(false, nil), passthroughResolver{})
	if _, err := NewGenerationCapability(nil, 9); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("nil materializer error = %v, want ErrInvalidCapability", err)
	}
	if _, err := NewGenerationCapability(materializer, 0); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("zero generation error = %v, want ErrInvalidCapability", err)
	}
	if err := (Value{}).Use(nil); err == nil {
		t.Fatal("Value.Use(nil) error = nil, want error")
	}
}

func TestMaterializerErrorsNeverContainRawOrPlaintext(t *testing.T) {
	const (
		raw       = "$secret://credential-value"
		plaintext = "credential-value"
	)
	scope := ownedScope()
	materializer := NewMaterializer(data_encryption.NewService(false, nil), referenceResolverFunc(
		func(context.Context, string) (string, error) {
			return "", errors.New("resolver exposed " + raw + " and " + plaintext)
		},
	))
	_, err := materializer.Materialize(context.Background(), scope, raw)
	if !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), raw) ||
		strings.Contains(err.Error(), plaintext) {
		t.Fatalf("reference Materialize() error = %v, want redacted ErrCredentialUnavailable", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = materializer.Materialize(cancelled, scope, raw)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), raw) ||
		strings.Contains(err.Error(), plaintext) {
		t.Fatalf("cancelled Materialize() error = %v, want redacted context cancellation", err)
	}
}
