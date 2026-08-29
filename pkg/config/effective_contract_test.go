package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEffectiveConfigContract(t *testing.T) {
	effective := EffectiveConfig{
		Config: Config{Debug: true},
		Provenance: Provenance{"proxy.max_in_flight": {
			Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
		}},
		Paths: RuntimePaths{DataDir: "/var/lib/apisix-go"},
	}
	request := LoadRequest{
		DefaultPath:  "conf/config-default.yaml",
		OverridePath: "conf/config.yaml",
		DefaultPaths: effective.Paths,
		Environment:  map[string]string{"APISIX_PORT": "9080"},
		CLIOverrides: map[string]any{"proxy.max_in_flight": 32},
	}

	if !effective.Config.Debug {
		t.Fatal("Config was lost")
	}
	if got := effective.Provenance["proxy.max_in_flight"]; got != (FieldSource{
		Kind: SourceCLI, Origin: "proxy.max_in_flight", Explicit: true,
	}) {
		t.Fatalf("Provenance source = %#v", got)
	}
	if request.DefaultPaths != effective.Paths ||
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
