package config

import (
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestExtensionSchemaIndexHonorsTagsKindsAndAliases(t *testing.T) {
	type child struct {
		Leaf string `mapstructure:"leaf"`
	}
	type synthetic struct {
		Nested     child            `mapstructure:"nested"`
		Pointer    *child           `mapstructure:"pointer"`
		Mapping    map[string]child `mapstructure:"mapping"`
		Sequence   []child          `mapstructure:"sequence"`
		Array      [1]child         `mapstructure:"array"`
		Scalar     string           `mapstructure:"scalar,omitempty"`
		Fallback   string           `mapstructure:",remain"`
		Skipped    string           `mapstructure:"-"`
		unexported string           `mapstructure:"unexported"` //nolint:unused // Verifies unexported fields are skipped.
	}
	index, err := buildStaticSchemaIndex(reflect.TypeFor[synthetic]())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"nested.leaf", "pointer", "mapping", "sequence", "array", "scalar", "Fallback"} {
		if _, ok := index.byPath[path]; !ok {
			t.Errorf("schema path %q is missing", path)
		}
	}
	for _, path := range []string{"nested", "pointer.leaf", "mapping.leaf", "sequence.leaf", "array.leaf", "Skipped", "unexported"} {
		if _, ok := index.byPath[path]; ok {
			t.Errorf("schema path %q must not be indexed", path)
		}
	}

	production, err := buildStaticSchemaIndex(reflect.TypeFor[Config]())
	if err != nil {
		t.Fatal(err)
	}
	for path, alias := range map[string]string{
		"proxy.max_in_flight": "APISIXGO_PROXY_MAX_IN_FLIGHT",
		"ext-plugin.cmd":      "APISIXGO_EXT_PLUGIN_CMD",
	} {
		if got := production.byAlias[alias]; got != path {
			t.Errorf("alias %s = %q, want %q", alias, got, path)
		}
	}
}

func TestExtensionSchemaIndexRejectsCollisionsDeterministically(t *testing.T) {
	t.Run("path collision", func(t *testing.T) {
		type collision struct {
			First  string `mapstructure:"same"`
			Second string `mapstructure:"same"`
		}
		for range 2 {
			_, err := buildStaticSchemaIndex(reflect.TypeFor[collision]())
			if err == nil || err.Error() != "static configuration path collision: same" {
				t.Fatalf("error = %v", err)
			}
		}
	})

	t.Run("environment alias collision", func(t *testing.T) {
		type collision struct {
			Hyphen     string `mapstructure:"foo-bar"`
			Underscore string `mapstructure:"foo_bar"`
		}
		for range 2 {
			_, err := buildStaticSchemaIndex(reflect.TypeFor[collision]())
			want := "static configuration environment alias collision: " +
				"APISIXGO_FOO_BAR maps to foo-bar and foo_bar"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v", err)
			}
		}
	})

	t.Run("reserved path collision", func(t *testing.T) {
		type runtimePaths struct {
			DataDir string `mapstructure:"data_dir"`
		}
		type apisixGo struct {
			RuntimePaths runtimePaths `mapstructure:"runtime_paths"`
		}
		type collision struct {
			ApisixGo apisixGo `mapstructure:"apisix_go"`
		}
		_, err := buildStaticSchemaIndex(reflect.TypeFor[collision]())
		if err == nil || err.Error() != "static configuration path collision: apisix_go.runtime_paths.data_dir" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("reserved environment alias collision", func(t *testing.T) {
		type runtimePaths struct {
			DataDir string `mapstructure:"data_dir"`
		}
		type collision struct {
			RuntimePaths runtimePaths `mapstructure:"runtime_paths"`
		}
		_, err := buildStaticSchemaIndex(reflect.TypeFor[collision]())
		want := "static configuration environment alias collision: APISIXGO_RUNTIME_PATHS_DATA_DIR " +
			"maps to apisix_go.runtime_paths.data_dir and runtime_paths.data_dir"
		if err == nil || err.Error() != want {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestExtensionReservedRuntimePathsUseOnlyShortAliases(t *testing.T) {
	root := mustNodeFromAny(map[string]any{}, FieldSource{Kind: SourceBuiltin, Origin: "defaults"})
	env := map[string]string{
		"APISIXGO_RUNTIME_PATHS_DATA_DIR":    "/env/data",
		"APISIXGO_RUNTIME_PATHS_RUNTIME_DIR": "/env/run",
		"APISIXGO_RUNTIME_PATHS_LOG_DIR":     "/env/log",
		"APISIXGO_RUNTIME_PATHS_TEMP_DIR":    "/env/tmp",
	}
	if err := applyAPISIXGO(root, env); err != nil {
		t.Fatal(err)
	}
	paths := root.mapping["apisix_go"].mapping["runtime_paths"].mapping
	for key, want := range map[string]string{
		"data_dir": "/env/data", "runtime_dir": "/env/run", "log_dir": "/env/log", "temp_dir": "/env/tmp",
	} {
		if got := paths[key].scalar; got != want {
			t.Errorf("%s = %#v, want %q", key, got, want)
		}
	}

	err := applyAPISIXGO(root, map[string]string{"APISIXGO_APISIX_GO_RUNTIME_PATHS_DATA_DIR": "secret-value"})
	want := "APISIXGO_APISIX_GO_RUNTIME_PATHS_DATA_DIR does not map to a static configuration field"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v", err)
	}
}

func TestAPISIXGOAppliesKnownStringValuesWithoutAmbientReads(t *testing.T) {
	t.Setenv("APISIXGO_DEBUG", "ambient-must-not-be-read")
	root := mustNodeFromAny(map[string]any{
		"debug": false,
		"proxy": map[string]any{"max_in_flight": 32},
	}, FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
	env := map[string]string{
		"APISIXGO_PROXY_MAX_IN_FLIGHT": "64",
		"APISIXGO_EXT_PLUGIN_CMD":      "",
		"UNRELATED":                    "ignored",
	}
	envBefore := cloneStringMap(env)
	if err := applyAPISIXGO(root, env); err != nil {
		t.Fatal(err)
	}
	if got := root.mapping["debug"].scalar; got != false {
		t.Fatalf("ambient environment changed debug: %#v", got)
	}
	if got := root.mapping["proxy"].mapping["max_in_flight"].scalar; got != "64" {
		t.Fatalf("numeric-looking env value = %#v", got)
	}
	if got := root.mapping["ext-plugin"].mapping["cmd"].scalar; got != "" {
		t.Fatalf("explicit empty env value = %#v", got)
	}
	for path, name := range map[string]string{
		"proxy.max_in_flight": "APISIXGO_PROXY_MAX_IN_FLIGHT",
		"ext-plugin.cmd":      "APISIXGO_EXT_PLUGIN_CMD",
	} {
		got := flattenProvenance(root)[path]
		want := FieldSource{Kind: SourceAPISIXGOEnv, Origin: name, Explicit: true}
		if got != want {
			t.Errorf("provenance[%s] = %+v, want %+v", path, got, want)
		}
	}
	if got, want := flattenProvenance(root)["ext-plugin"], (FieldSource{
		Kind: SourceAPISIXGOEnv, Origin: "APISIXGO_EXT_PLUGIN_CMD", Explicit: true,
	}); got != want {
		t.Errorf("provenance[ext-plugin] = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(env, envBefore) {
		t.Fatalf("environment input mutated: %#v", env)
	}
}

func TestAPISIXGOFailureIsSortedAtomicAndDoesNotLeakValues(t *testing.T) {
	const secret = "C3_ENV_SENTINEL_165C"
	root := mustNodeFromAny(map[string]any{
		"debug": false,
		"proxy": map[string]any{"max_in_flight": 32},
	}, FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
	rootBefore := cloneNode(root)
	env := map[string]string{
		"APISIXGO_PROXY_MAX_IN_FLIGHT": "64",
		"APISIXGO_Z_UNKNOWN":           secret,
		"APISIXGO_A_UNKNOWN":           secret,
	}
	envBefore := cloneStringMap(env)
	err := applyAPISIXGO(root, env)
	if err == nil || err.Error() != "APISIXGO_A_UNKNOWN does not map to a static configuration field" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked value: %q", err)
	}
	if !reflect.DeepEqual(root, rootBefore) || !reflect.DeepEqual(env, envBefore) {
		t.Fatalf("failure mutated inputs\nroot=%#v\nenv=%#v", root, env)
	}
}

func TestCLIAcceptsKnownLeafAndNestedValuesWithExactConversion(t *testing.T) {
	root := mustNodeFromAny(map[string]any{
		"proxy":       map[string]any{"max_in_flight": 32},
		"plugin_attr": map[string]any{},
	}, FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
	root.mapping["proxy"].pathBase = "/defaults"
	nested := map[string]any{"custom": map[string]any{"limit": uint64(math.MaxUint64), "items": []string{"a"}}}
	cli := map[string]any{
		"proxy.max_in_flight":              int64(math.MinInt64),
		"apisix_go.runtime_paths.data_dir": "relative/data",
		"plugin_attr":                      nested,
		"deployment.etcd.user":             nil,
	}
	if err := applyCLIOverrides(root, cli); err != nil {
		t.Fatal(err)
	}
	if got := root.mapping["proxy"].mapping["max_in_flight"].scalar; got != json.Number("-9223372036854775808") {
		t.Fatalf("exact CLI integer = %#v", got)
	}
	if got := root.mapping["apisix_go"].mapping["runtime_paths"].mapping["data_dir"].scalar; got != "relative/data" {
		t.Fatalf("runtime data path = %#v", got)
	}
	runtimeLeaf := root.mapping["apisix_go"].mapping["runtime_paths"].mapping["data_dir"]
	for path, container := range map[string]*valueNode{
		"apisix_go":               root.mapping["apisix_go"],
		"apisix_go.runtime_paths": root.mapping["apisix_go"].mapping["runtime_paths"],
	} {
		if container.source != runtimeLeaf.source || container.pathBase != runtimeLeaf.pathBase {
			t.Errorf("%s metadata = %+v/%q, want %+v/%q", path,
				container.source, container.pathBase, runtimeLeaf.source, runtimeLeaf.pathBase)
		}
	}
	if proxy := root.mapping["proxy"]; proxy.source.Kind != SourceDefaultFile || proxy.pathBase != "/defaults" {
		t.Errorf("existing proxy metadata was overwritten: %+v/%q", proxy.source, proxy.pathBase)
	}
	if got := root.mapping["deployment"].mapping["etcd"].mapping["user"].kind; got != nodeNull {
		t.Fatalf("nil CLI kind = %d", got)
	}
	limit := root.mapping["plugin_attr"].mapping["custom"].mapping["limit"].scalar
	if limit != json.Number("18446744073709551615") {
		t.Fatalf("nested uint64 = %#v", limit)
	}
	nested["custom"].(map[string]any)["limit"] = uint64(1)
	nested["custom"].(map[string]any)["items"].([]string)[0] = "changed"
	gotAfterMutation := root.mapping["plugin_attr"].mapping["custom"].mapping["limit"].scalar
	if gotAfterMutation != json.Number("18446744073709551615") {
		t.Fatalf("later nested input mutation leaked: %#v", gotAfterMutation)
	}
	for _, path := range []string{"proxy.max_in_flight", "apisix_go.runtime_paths.data_dir", "plugin_attr", "deployment.etcd.user"} {
		want := FieldSource{Kind: SourceCLI, Origin: path, Explicit: true}
		if got := flattenProvenance(root)[path]; got != want {
			t.Errorf("provenance[%s] = %+v, want %+v", path, got, want)
		}
	}
}

func TestCLIRejectsInvalidPathsAndValuesAtomically(t *testing.T) {
	tests := []struct {
		name string
		root map[string]any
		cli  map[string]any
		want string
	}{
		{
			name: "unknown", root: map[string]any{"debug": false}, cli: map[string]any{"unknown.path": "x"},
			want: `unknown.path does not map to a static configuration field`,
		},
		{
			name: "empty path", root: map[string]any{}, cli: map[string]any{"": "x"},
			want: `configuration path "" is empty`,
		},
		{
			name: "empty segment", root: map[string]any{}, cli: map[string]any{"proxy..max_in_flight": "x"},
			want: `configuration path "proxy..max_in_flight" contains an empty segment`,
		},
		{
			name: "crosses non-mapping", root: map[string]any{"proxy": "scalar"},
			cli:  map[string]any{"proxy.max_in_flight": 64},
			want: `configuration path proxy.max_in_flight crosses a non-mapping value`,
		},
		{
			name: "unsupported value", root: map[string]any{"debug": false}, cli: map[string]any{"debug": 1.5},
			want: `configuration value type float64 is unsupported`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := mustNodeFromAny(test.root, FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"})
			rootBefore := cloneNode(root)
			cliBefore := cloneAnyMap(test.cli)
			err := applyCLIOverrides(root, test.cli)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if !reflect.DeepEqual(root, rootBefore) || !reflect.DeepEqual(test.cli, cliBefore) {
				t.Fatalf("failure mutated inputs\nroot=%#v\ncli=%#v", root, test.cli)
			}
		})
	}

	root := mustNodeFromAny(map[string]any{"debug": false}, FieldSource{Kind: SourceDefaultFile})
	rootBefore := cloneNode(root)
	cli := map[string]any{"debug": true, "zz_unknown": "C3_CLI_SENTINEL"}
	err := applyCLIOverrides(root, cli)
	if err == nil || strings.Contains(err.Error(), "C3_CLI_SENTINEL") || !reflect.DeepEqual(root, rootBefore) {
		t.Fatalf("mixed validation error/root = %v/%#v", err, root)
	}
}

func TestOverlayRemovedDeploymentProfileTombstone(t *testing.T) {
	const want = "deployment.profile was removed; use compatibility_target, security_profile, and qualification_profile"
	root := mustNodeFromAny(map[string]any{}, FieldSource{Kind: SourceDefaultFile})
	err := applyAPISIXGO(root, map[string]string{"APISIXGO_DEPLOYMENT_PROFILE": "traditional"})
	if err == nil || err.Error() != want {
		t.Fatalf("APISIXGO tombstone error = %v", err)
	}
	err = applyCLIOverrides(root, map[string]any{"deployment.profile": "traditional"})
	if err == nil || err.Error() != want {
		t.Fatalf("CLI tombstone error = %v", err)
	}
	if err := ValidateStaticOverridePath("deployment.profile"); err == nil || err.Error() != want {
		t.Fatalf("path tombstone error = %v", err)
	}
	index, err := buildStaticSchemaIndex(reflect.TypeFor[Config]())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.byPath["deployment.profile"]; ok {
		t.Fatal("removed path entered schema index")
	}
	if _, ok := index.byAlias["APISIXGO_DEPLOYMENT_PROFILE"]; ok {
		t.Fatal("removed alias entered schema index")
	}
}

func TestSetPathValidatesRootSegmentsCrossingsAndClonesValue(t *testing.T) {
	value := mustNodeFromAny(
		map[string]any{"nested": []string{"a"}},
		FieldSource{Kind: SourceCLI, Origin: "plugin_attr", Explicit: true},
	)
	root := mustNodeFromAny(map[string]any{}, FieldSource{Kind: SourceDefaultFile})
	if err := setPath(root, "plugin_attr", value); err != nil {
		t.Fatal(err)
	}
	value.mapping["nested"].sequence[0].scalar = "changed"
	if got := root.mapping["plugin_attr"].mapping["nested"].sequence[0].scalar; got != "a" {
		t.Fatalf("setPath aliased value: %#v", got)
	}
	intermediateValue := mustNodeFromAny("value", FieldSource{
		Kind: SourceCLI, Origin: "new.container.leaf", Explicit: true,
	})
	intermediateValue.pathBase = "/cli"
	if err := setPath(root, "new.container.leaf", intermediateValue); err != nil {
		t.Fatal(err)
	}
	for path, container := range map[string]*valueNode{
		"new":           root.mapping["new"],
		"new.container": root.mapping["new"].mapping["container"],
	} {
		if container.source != intermediateValue.source || container.pathBase != intermediateValue.pathBase {
			t.Errorf("%s metadata = %+v/%q, want %+v/%q", path,
				container.source, container.pathBase, intermediateValue.source, intermediateValue.pathBase)
		}
	}

	for name, test := range map[string]struct {
		root *valueNode
		path string
		want string
	}{
		"nil root":      {root: nil, path: "debug", want: "configuration root must be a mapping"},
		"scalar root":   {root: &valueNode{kind: nodeScalar}, path: "debug", want: "configuration root must be a mapping"},
		"empty path":    {root: mustNodeFromAny(map[string]any{}, FieldSource{}), path: "", want: `configuration path "" is empty`},
		"empty segment": {root: mustNodeFromAny(map[string]any{}, FieldSource{}), path: "a..b", want: `configuration path "a..b" contains an empty segment`},
		"crossing":      {root: mustNodeFromAny(map[string]any{"a": "scalar"}, FieldSource{}), path: "a.b", want: "configuration path a.b crosses a non-mapping value"},
	} {
		t.Run(name, func(t *testing.T) {
			err := setPath(test.root, test.path, value)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	cloned := make(map[string]string, len(input))
	maps.Copy(cloned, input)
	return cloned
}

func cloneAnyMap(input map[string]any) map[string]any {
	cloned := make(map[string]any, len(input))
	maps.Copy(cloned, input)
	return cloned
}
