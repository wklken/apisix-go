package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestEffectiveConfigContract(t *testing.T) {
	manifest := qualifiedProfileTestManifest(t)
	effective := EffectiveConfig{
		Config: Config{Debug: true},
		Provenance: Provenance{"proxy.max_in_flight": {
			Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
		}},
		Profiles: ProfileSelection{
			Compatibility: CompatibilityAPISIX317,
			Security:      SecurityCompat,
		},
		Paths: RuntimePaths{DataDir: "/var/lib/apisix-go"},
	}
	request := LoadRequest{
		DefaultPath:  "conf/config-default.yaml",
		OverridePath: "conf/config.yaml",
		DefaultPaths: effective.Paths,
		Environment:  map[string]string{"APISIX_PORT": "9080"},
		CLIOverrides: map[string]any{"proxy.max_in_flight": 32},
		Manifest:     manifest,
	}

	if !effective.Config.Debug {
		t.Fatal("Config was lost")
	}
	if got := effective.Provenance["proxy.max_in_flight"]; got != (FieldSource{
		Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
	}) {
		t.Fatalf("Provenance source = %#v", got)
	}
	if effective.Profiles.Compatibility != CompatibilityAPISIX317 {
		t.Fatal("profile was lost")
	}
	if request.Manifest != manifest || request.DefaultPaths != effective.Paths ||
		request.Environment["APISIX_PORT"] != "9080" || request.CLIOverrides["proxy.max_in_flight"] != 32 {
		t.Fatalf("LoadRequest lost explicit inputs: %#v", request)
	}

	encoded, err := json.Marshal(FieldSource{Kind: SourceAPISIXGOEnv, Origin: "APISIXGO_DEBUG", Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"kind":"apisixgo_env","origin":"APISIXGO_DEBUG","explicit":true}` {
		t.Fatalf("FieldSource JSON = %s", encoded)
	}

	wantSources := map[SourceKind]string{
		SourceBuiltin:      "builtin",
		SourceDefaultFile:  "default_file",
		SourceOverrideFile: "override_file",
		SourceAPISIXEnv:    "apisix_env",
		SourceAPISIXGOEnv:  "apisixgo_env",
		SourceCLI:          "cli",
	}
	for source, want := range wantSources {
		if string(source) != want {
			t.Fatalf("source kind = %q, want %q", source, want)
		}
	}

	typeOfPaths := reflect.TypeFor[RuntimePaths]()
	for fieldName, wantTag := range map[string]string{
		"DataDir": "data_dir", "RuntimeDir": "runtime_dir", "LogDir": "log_dir", "TempDir": "temp_dir",
	} {
		field, ok := typeOfPaths.FieldByName(fieldName)
		if !ok {
			t.Fatalf("RuntimePaths.%s is missing", fieldName)
		}
		if field.Tag.Get("mapstructure") != wantTag || field.Tag.Get("json") != wantTag {
			t.Fatalf("RuntimePaths.%s tags = %q", fieldName, field.Tag)
		}
	}
}

func TestDefaultRuntimePaths(t *testing.T) {
	paths, err := DefaultRuntimePaths()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
		"log_dir": paths.LogDir, "temp_dir": paths.TempDir,
	} {
		if path == "" || !filepath.IsAbs(path) {
			t.Fatalf("%s = %q, want non-empty absolute path", name, path)
		}
	}
}

func TestJournalPath(t *testing.T) {
	t.Run("absolute data directory", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "data")
		effective := &EffectiveConfig{Paths: RuntimePaths{DataDir: dataDir}}
		want := filepath.Join(dataDir, "apisix-go-store.db")
		if got := JournalPath(effective); got != want {
			t.Fatalf("JournalPath() = %q, want %q", got, want)
		}
	})

	t.Run("rejects missing or relative data directory", func(t *testing.T) {
		for name, effective := range map[string]*EffectiveConfig{
			"nil config": nil,
			"empty path": {},
			"relative path": {
				Paths: RuntimePaths{DataDir: "relative"},
			},
		} {
			t.Run(name, func(t *testing.T) {
				if got := JournalPath(effective); got != "" {
					t.Fatalf("JournalPath() = %q, want empty", got)
				}
			})
		}
	})
}

func TestValidateQualificationPlugins(t *testing.T) {
	validEmptySelection := ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security:      SecurityCompat,
	}

	t.Run("empty qualification still validates selection", func(t *testing.T) {
		manifest, err := capability.Load()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateQualificationPlugins([]string{"not-checked"}, validEmptySelection, manifest); err != nil {
			t.Fatalf("ValidateQualificationPlugins() error = %v", err)
		}

		invalid := validEmptySelection
		invalid.Compatibility = "unsupported"
		err = ValidateQualificationPlugins(nil, invalid, manifest)
		if err == nil || err.Error() != "compatibility_target is unsupported" {
			t.Fatalf("ValidateQualificationPlugins() error = %v", err)
		}
	})

	t.Run("nil manifest returns selection error", func(t *testing.T) {
		err := ValidateQualificationPlugins(nil, validEmptySelection, nil)
		if err == nil || err.Error() != "capability manifest must not be nil" {
			t.Fatalf("ValidateQualificationPlugins() error = %v", err)
		}
	})

	t.Run("production manifest fails at evidence qualification", func(t *testing.T) {
		manifest, err := capability.Load()
		if err != nil {
			t.Fatal(err)
		}
		selection := qualifiedSelection()
		err = ValidateQualificationPlugins(nil, selection, manifest)
		if err == nil || !strings.Contains(err.Error(), "unqualified required plugins") {
			t.Fatalf("ValidateQualificationPlugins() error = %v", err)
		}
	})

	t.Run("exact required order passes and reordered membership fails without mutation", func(t *testing.T) {
		manifest := qualifiedProfileTestManifest(t)
		profile, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
		if !ok {
			t.Fatal("qualification profile is missing")
		}
		manifestBefore := append([]string(nil), profile.RequiredPlugins...)
		enabled := append([]string(nil), profile.RequiredPlugins...)
		enabledBefore := append([]string(nil), enabled...)
		if err := ValidateQualificationPlugins(enabled, qualifiedSelection(), manifest); err != nil {
			t.Fatalf("exact membership error = %v", err)
		}
		if !slices.Equal(enabled, enabledBefore) {
			t.Fatalf("enabled input mutated: got %v, want %v", enabled, enabledBefore)
		}

		slices.Reverse(enabled)
		enabledBefore = append([]string(nil), enabled...)
		err := ValidateQualificationPlugins(enabled, qualifiedSelection(), manifest)
		want := "qualification_profile http-data-plane-v1: plugins must exactly match required order"
		if err == nil || err.Error() != want {
			t.Fatalf("reordered membership error = %v, want %q", err, want)
		}
		if !slices.Equal(enabled, enabledBefore) {
			t.Fatalf("reordered enabled input mutated: got %v, want %v", enabled, enabledBefore)
		}
		assertQualificationRequiredPlugins(t, manifest, manifestBefore)
	})

	t.Run("stable sorted set difference without mutation", func(t *testing.T) {
		manifest := qualifiedProfileTestManifest(t)
		profile, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
		if !ok || len(profile.RequiredPlugins) < 2 {
			t.Fatalf("qualification profile = %#v", profile)
		}
		manifestBefore := append([]string(nil), profile.RequiredPlugins...)
		enabled := append([]string(nil), profile.RequiredPlugins[2:]...)
		enabled = append(enabled, "zz-extra", "aa-extra")
		enabledBefore := append([]string(nil), enabled...)
		err := ValidateQualificationPlugins(enabled, qualifiedSelection(), manifest)
		want := "qualification_profile http-data-plane-v1: plugins missing count 2; unexpected count 2"
		if err == nil || err.Error() != want {
			t.Fatalf("ValidateQualificationPlugins() error = %v, want %q", err, want)
		}
		if !slices.Equal(enabled, enabledBefore) {
			t.Fatalf("enabled input mutated: got %v, want %v", enabled, enabledBefore)
		}
		assertQualificationRequiredPlugins(t, manifest, manifestBefore)
	})

	t.Run("duplicates fail closed and are sorted", func(t *testing.T) {
		manifest := qualifiedProfileTestManifest(t)
		profile, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
		if !ok || len(profile.RequiredPlugins) < 2 {
			t.Fatalf("qualification profile = %#v", profile)
		}
		manifestBefore := append([]string(nil), profile.RequiredPlugins...)
		enabled := append([]string(nil), profile.RequiredPlugins...)
		enabled = append(enabled, profile.RequiredPlugins[1], profile.RequiredPlugins[0])
		enabledBefore := append([]string(nil), enabled...)
		err := ValidateQualificationPlugins(enabled, qualifiedSelection(), manifest)
		want := "qualification_profile http-data-plane-v1: plugins duplicate enabled count 2"
		if err == nil || err.Error() != want {
			t.Fatalf("ValidateQualificationPlugins() error = %v, want %q", err, want)
		}
		if !slices.Equal(enabled, enabledBefore) {
			t.Fatalf("enabled input mutated: got %v, want %v", enabled, enabledBefore)
		}
		assertQualificationRequiredPlugins(t, manifest, manifestBefore)
	})
}

func qualifiedSelection() ProfileSelection {
	return ProfileSelection{
		Compatibility: CompatibilityAPISIX317,
		Security:      SecurityStrict,
		Qualification: QualificationHTTPDataPlaneV1,
	}
}

func assertQualificationRequiredPlugins(t *testing.T, manifest *capability.Manifest, want []string) {
	t.Helper()
	profile, ok := manifest.Qualification(string(QualificationHTTPDataPlaneV1))
	if !ok {
		t.Fatal("qualification profile is missing")
	}
	if !slices.Equal(profile.RequiredPlugins, want) {
		t.Fatalf("manifest required plugins mutated: got %v, want %v", profile.RequiredPlugins, want)
	}
}
