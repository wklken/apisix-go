package capability

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSecretDeclarationCatalogIsDeterministicAndDefensive(t *testing.T) {
	declarations := []SecretDeclaration{
		{Factory: "request-id", Source: SecretPluginMetadata, Field: "metadata.*.token"},
		{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret"},
		{Factory: "request-id", Source: SecretConsumerConfig, Field: "consumer.key"},
	}
	catalog := mustDeclarationCatalog(t, declarations)
	want := []SecretDeclaration{
		{Factory: "request-id", Source: SecretConsumerConfig, Field: "consumer.key"},
		{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret"},
		{Factory: "request-id", Source: SecretPluginMetadata, Field: "metadata.*.token"},
	}
	if got := catalog.Declarations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Declarations() = %#v, want %#v", got, want)
	}
	got := catalog.Declarations()
	got[0].Field = "mutated"
	if declarations := catalog.Declarations(); !reflect.DeepEqual(declarations, want) {
		t.Fatalf("Declarations() exposed mutable storage: %#v", declarations)
	}
	if declaration, ok := catalog.Lookup("request-id", SecretPluginConfig, "auth.secret"); !ok {
		t.Fatalf("Lookup() = %#v/%v, want config declaration", declaration, ok)
	}
	var visited []SecretDeclaration
	catalog.ForEach("request-id", SecretPluginConfig, func(declaration SecretDeclaration) {
		visited = append(visited, declaration)
	})
	if !reflect.DeepEqual(visited, []SecretDeclaration{want[1]}) {
		t.Fatalf("ForEach() = %#v", visited)
	}

	shuffled := slices.Clone(declarations)
	slices.Reverse(shuffled)
	if digest := mustDeclarationCatalog(t, shuffled).Digest(); digest != catalog.Digest() {
		t.Fatalf("Digest() changed with declaration ordering: %x != %x", catalog.Digest(), digest)
	}
}

func TestSecretDeclarationCatalogDigestIncludesIdentity(t *testing.T) {
	base := []SecretDeclaration{{
		Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret",
	}}
	baseDigest := mustDeclarationCatalog(t, base).Digest()
	for name, declaration := range map[string]SecretDeclaration{
		"factory": {Factory: "other", Source: SecretPluginConfig, Field: "auth.secret"},
		"source":  {Factory: "request-id", Source: SecretConsumerConfig, Field: "auth.secret"},
		"field":   {Factory: "request-id", Source: SecretPluginConfig, Field: "other.secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if digest := mustDeclarationCatalog(t, []SecretDeclaration{declaration}).Digest(); digest == baseDigest {
				t.Fatal("Digest() did not change")
			}
		})
	}
}

func TestSecretDeclarationCatalogRejectsInvalidDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		declarations []SecretDeclaration
		want         string
	}{
		{
			name:         "blank factory",
			declarations: []SecretDeclaration{{Source: SecretPluginConfig, Field: "token"}},
			want:         "factory",
		},
		{
			name:         "unknown source",
			declarations: []SecretDeclaration{{Factory: "p", Source: "other", Field: "token"}},
			want:         "unknown source",
		},
		{
			name:         "blank field",
			declarations: []SecretDeclaration{{Factory: "p", Source: SecretPluginConfig, Field: " "}},
			want:         "canonical wildcard path",
		},
		{
			name:         "empty segment",
			declarations: []SecretDeclaration{{Factory: "p", Source: SecretPluginConfig, Field: "auth..token"}},
			want:         "canonical wildcard path",
		},
		{
			name:         "noncanonical wildcard",
			declarations: []SecretDeclaration{{Factory: "p", Source: SecretPluginConfig, Field: "auth[*].token"}},
			want:         "canonical wildcard path",
		},
		{
			name:         "terminal wildcard",
			declarations: []SecretDeclaration{{Factory: "p", Source: SecretPluginConfig, Field: "auth.*"}},
			want:         "canonical wildcard path",
		},
		{
			name: "duplicate",
			declarations: []SecretDeclaration{
				{Factory: "p", Source: SecretPluginConfig, Field: "token"},
				{Factory: "p", Source: SecretPluginConfig, Field: "token"},
			},
			want: "duplicate factory/source/field tuple",
		},
		{
			name: "wildcard overlap",
			declarations: []SecretDeclaration{
				{Factory: "p", Source: SecretPluginConfig, Field: "auth.*.token"},
				{Factory: "p", Source: SecretPluginConfig, Field: "auth.primary.token"},
			},
			want: "overlaps declared field",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newSecretDeclarationCatalog(test.declarations)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("newSecretDeclarationCatalog() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuiltInSecretDeclarationsRemainAvailable(t *testing.T) {
	catalog, err := NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(catalog.Declarations()); got != 98 {
		t.Fatalf("declaration count = %d, want 98", got)
	}
	for _, key := range []SecretDeclaration{
		{Factory: "jwt-auth", Source: SecretPluginConfig, Field: "private_key"},
		{Factory: "key-auth", Source: SecretConsumerConfig, Field: "key"},
		{Factory: "kafka-proxy", Source: SecretPluginConfig, Field: "sasl.password"},
	} {
		if _, ok := catalog.Lookup(key.Factory, key.Source, key.Field); !ok {
			t.Fatalf("built-in declaration is missing: %#v", key)
		}
	}
}

func mustDeclarationCatalog(t *testing.T, declarations []SecretDeclaration) *SecretDeclarationCatalog {
	t.Helper()
	catalog, err := newSecretDeclarationCatalog(declarations)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
