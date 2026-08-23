package data_encryption

import (
	"reflect"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

type legacyDeclarationKey struct {
	factory string
	field   string
}

func TestSecretDeclarationCatalogParityWithLegacyTables(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}

	wantConfig := make(map[legacyDeclarationKey]struct{})
	wantOptionalConfig := make(map[legacyDeclarationKey]struct{})
	wantStrictConfig := make(map[legacyDeclarationKey]struct{})
	for factory, fields := range pluginFields {
		for _, field := range fields {
			key := legacyDeclarationKey{factory: factory, field: field}
			wantConfig[key] = struct{}{}
			if !slices.Contains(strictPluginFields[factory], field) {
				wantOptionalConfig[key] = struct{}{}
			}
		}
	}
	for factory, fields := range strictPluginFields {
		for _, field := range fields {
			wantStrictConfig[legacyDeclarationKey{factory: factory, field: field}] = struct{}{}
		}
	}

	wantMetadata := make(map[legacyDeclarationKey]struct{})
	wantOptionalMetadata := make(map[legacyDeclarationKey]struct{})
	wantStrictMetadata := make(map[legacyDeclarationKey]struct{})
	for factory, fields := range pluginMetadataFields {
		for _, field := range fields {
			key := legacyDeclarationKey{factory: factory, field: field}
			wantMetadata[key] = struct{}{}
			if !slices.Contains(strictPluginMetadataFields[factory], field) {
				wantOptionalMetadata[key] = struct{}{}
			}
		}
	}
	for factory, fields := range strictPluginMetadataFields {
		for _, field := range fields {
			wantStrictMetadata[legacyDeclarationKey{factory: factory, field: field}] = struct{}{}
		}
	}

	gotConfig := make(map[legacyDeclarationKey]struct{})
	gotOptionalConfig := make(map[legacyDeclarationKey]struct{})
	gotStrictConfig := make(map[legacyDeclarationKey]struct{})
	gotMetadata := make(map[legacyDeclarationKey]struct{})
	gotOptionalMetadata := make(map[legacyDeclarationKey]struct{})
	gotStrictMetadata := make(map[legacyDeclarationKey]struct{})
	for _, declaration := range catalog.Declarations() {
		key := legacyDeclarationKey{factory: declaration.Factory, field: declaration.Field}
		switch declaration.Source {
		case capability.SecretPluginConfig:
			gotConfig[key] = struct{}{}
			if declaration.Strict {
				gotStrictConfig[key] = struct{}{}
			} else {
				gotOptionalConfig[key] = struct{}{}
			}
		case capability.SecretPluginMetadata:
			gotMetadata[key] = struct{}{}
			if declaration.Strict {
				gotStrictMetadata[key] = struct{}{}
			} else {
				gotOptionalMetadata[key] = struct{}{}
			}
		default:
			t.Fatalf("catalog contains unknown declaration source %q", declaration.Source)
		}
	}

	checks := []struct {
		name string
		got  map[legacyDeclarationKey]struct{}
		want map[legacyDeclarationKey]struct{}
	}{
		{name: "config", got: gotConfig, want: wantConfig},
		{name: "config optional", got: gotOptionalConfig, want: wantOptionalConfig},
		{name: "config strict", got: gotStrictConfig, want: wantStrictConfig},
		{name: "metadata", got: gotMetadata, want: wantMetadata},
		{name: "metadata optional", got: gotOptionalMetadata, want: wantOptionalMetadata},
		{name: "metadata strict", got: gotStrictMetadata, want: wantStrictMetadata},
	}
	for _, check := range checks {
		if !reflect.DeepEqual(check.got, check.want) {
			t.Errorf("%s declarations differ from legacy table: got %#v, want %#v", check.name, check.got, check.want)
		}
	}
}
