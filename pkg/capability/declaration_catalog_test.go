package capability

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSecretDeclarationCatalogIsDeterministicAndDefensive(t *testing.T) {
	manifest := testManifest()
	manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
		{Factory: "request-id", Source: SecretPluginMetadata, Field: "metadata.*.token", Strict: true},
		{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret"},
		{Factory: "request-id", Source: SecretConsumerConfig, Field: "consumer.key"},
	}
	loaded := parseManifest(t, manifest)
	catalog, err := NewSecretDeclarationCatalog(loaded)
	if err != nil {
		t.Fatal(err)
	}

	want := []SecretDeclaration{
		{Factory: "request-id", Source: SecretConsumerConfig, Field: "consumer.key"},
		{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret"},
		{Factory: "request-id", Source: SecretPluginMetadata, Field: "metadata.*.token", Strict: true},
	}
	if got := catalog.Declarations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Declarations() = %#v, want %#v", got, want)
	}
	got := catalog.Declarations()
	got[0].Field = "mutated"
	if declarations := catalog.Declarations(); !reflect.DeepEqual(declarations, want) {
		t.Fatalf("Declarations() exposed mutable storage: %#v", declarations)
	}
	if declaration, ok := catalog.Lookup("request-id", SecretPluginConfig, "auth.secret"); !ok || declaration.Strict {
		t.Fatalf("Lookup() = %#v/%v, want optional config declaration", declaration, ok)
	}
	if _, ok := catalog.Lookup("request-id", SecretPluginConfig, "missing"); ok {
		t.Fatal("Lookup() unexpectedly found an undeclared field")
	}
	var visited []SecretDeclaration
	catalog.ForEach("request-id", SecretPluginConfig, func(declaration SecretDeclaration) {
		visited = append(visited, declaration)
	})
	if want := []SecretDeclaration{{
		Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret",
	}}; !reflect.DeepEqual(visited, want) {
		t.Fatalf("ForEach() = %#v, want %#v", visited, want)
	}
	visited[0].Field = "mutated"
	var unchanged SecretDeclaration
	catalog.ForEach("request-id", SecretPluginConfig, func(declaration SecretDeclaration) {
		unchanged = declaration
	})
	if unchanged.Field != "auth.secret" {
		t.Fatalf("ForEach() exposed mutable declaration: %#v", unchanged)
	}
	catalog.ForEach("request-id", SecretDeclarationSource("missing"), func(SecretDeclaration) {
		t.Fatal("ForEach() visited an undeclared source")
	})

	shuffled := testManifest()
	shuffled.Plugins[0].SecretDeclarations = []SecretDeclaration{
		{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret"},
		{Factory: "request-id", Source: SecretConsumerConfig, Field: "consumer.key"},
		{Factory: "request-id", Source: SecretPluginMetadata, Field: "metadata.*.token", Strict: true},
	}
	shuffledLoaded := parseManifest(t, shuffled)
	shuffledCatalog, err := NewSecretDeclarationCatalog(shuffledLoaded)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Digest() != shuffledCatalog.Digest() {
		t.Fatalf("Digest() changed with declaration ordering: %x != %x", catalog.Digest(), shuffledCatalog.Digest())
	}
}

func TestSecretDeclarationCatalogAcceptsConsumerConfig(t *testing.T) {
	manifest := testManifest()
	manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
		Factory: "request-id", Source: SecretConsumerConfig, Field: "username",
	}}
	catalog := mustDeclarationCatalog(t, manifest)
	if declaration, ok := catalog.Lookup(
		"request-id",
		SecretConsumerConfig,
		"username",
	); !ok || declaration.Strict {
		t.Fatalf("Lookup() = %#v/%v, want optional consumer declaration", declaration, ok)
	}
}

func TestSecretDeclarationCatalogDigestIncludesPolicyAndSource(t *testing.T) {
	base := testManifest()
	alternateFactory := base.Plugins[0].Factories[0]
	alternateFactory.Key = "request-id-alternate"
	base.Plugins[0].Factories = append(base.Plugins[0].Factories, alternateFactory)
	base.Plugins[0].SecretDeclarations = []SecretDeclaration{{
		Factory: "request-id", Source: SecretPluginConfig, Field: "auth.secret",
	}}
	baseCatalog := mustDeclarationCatalog(t, base)

	for name, mutate := range map[string]func(*Manifest){
		"strict": func(manifest *Manifest) {
			manifest.Plugins[0].SecretDeclarations[0].Strict = true
		},
		"source": func(manifest *Manifest) {
			manifest.Plugins[0].SecretDeclarations[0].Source = SecretConsumerConfig
		},
		"field": func(manifest *Manifest) {
			manifest.Plugins[0].SecretDeclarations[0].Field = "other.secret"
		},
		"factory": func(manifest *Manifest) {
			manifest.Plugins[0].SecretDeclarations[0].Factory = "request-id-alternate"
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := base
			manifest.Plugins = append([]PluginCapability(nil), base.Plugins...)
			manifest.Plugins[0].SecretDeclarations = append(
				[]SecretDeclaration(nil),
				base.Plugins[0].SecretDeclarations...,
			)
			mutate(&manifest)
			catalog := mustDeclarationCatalog(t, manifest)
			if catalog.Digest() == baseCatalog.Digest() {
				t.Fatal("Digest() did not change after declaration mutation")
			}
		})
	}
}

func TestParseRejectsInvalidSecretDeclarations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manifest)
		want   string
	}{
		{
			name: "unknown source",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "request-id", Source: SecretDeclarationSource("other"), Field: "token",
				}}
			},
			want: "unknown source",
		},
		{
			name: "factory not owned",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "missing", Source: SecretPluginConfig, Field: "token",
				}}
			},
			want: "not owned",
		},
		{
			name: "blank field",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "request-id", Source: SecretPluginConfig, Field: " ",
				}}
			},
			want: "canonical wildcard path",
		},
		{
			name: "empty path segment",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "request-id", Source: SecretPluginConfig, Field: "auth..token",
				}}
			},
			want: "canonical wildcard path",
		},
		{
			name: "noncanonical wildcard",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "request-id", Source: SecretPluginConfig, Field: "auth[*].token",
				}}
			},
			want: "canonical wildcard path",
		},
		{
			name: "terminal wildcard",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
					Factory: "request-id", Source: SecretPluginConfig, Field: "auth.*",
				}}
			},
			want: "canonical wildcard path",
		},
		{
			name: "duplicate tuple",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "token"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "token"},
				}
			},
			want: "duplicate factory/source/field tuple",
		},
		{
			name: "conflicting policy",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "token"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "token", Strict: true},
				}
			},
			want: "conflicting strict policy",
		},
		{
			name: "case-insensitive duplicate path",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "headers.Authorization"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "headers.authorization"},
				}
			},
			want: "duplicate factory/source/field tuple",
		},
		{
			name: "wildcard overlap",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.*.token"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.primary.token"},
				}
			},
			want: "overlaps declared field",
		},
		{
			name: "prefix overlap",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.token"},
				}
			},
			want: "overlaps declared field",
		},
		{
			name: "overlap with conflicting policy",
			mutate: func(manifest *Manifest) {
				manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.*.token"},
					{Factory: "request-id", Source: SecretPluginConfig, Field: "auth.primary.token", Strict: true},
				}
			},
			want: "conflicting strict policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := testManifest()
			tt.mutate(&manifest)
			_, err := Parse(marshalManifest(t, manifest))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestManifestQueriesCloneSecretDeclarations(t *testing.T) {
	manifest := testManifest()
	manifest.Plugins[0].SecretDeclarations = []SecretDeclaration{{
		Factory: "request-id", Source: SecretPluginConfig, Field: "token",
	}}
	loaded := parseManifest(t, manifest)
	plugin, ok := loaded.Plugin("request-id")
	if !ok {
		t.Fatal("request-id factory missing")
	}
	plugin.SecretDeclarations[0].Field = "mutated"
	again, _ := loaded.Plugin("request-id")
	if slices.ContainsFunc(again.SecretDeclarations, func(declaration SecretDeclaration) bool {
		return declaration.Field == "mutated"
	}) {
		t.Fatal("Plugin() exposed mutable secret declaration storage")
	}
}

func mustDeclarationCatalog(t *testing.T, manifest Manifest) *SecretDeclarationCatalog {
	t.Helper()
	return mustDeclarationCatalogFromLoaded(t, parseManifest(t, manifest))
}

func mustDeclarationCatalogFromLoaded(t *testing.T, manifest *Manifest) *SecretDeclarationCatalog {
	t.Helper()
	catalog, err := NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
