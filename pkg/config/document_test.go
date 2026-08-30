package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDocumentPreservesPresenceKindsAndExactNumbers(t *testing.T) {
	pathBase := filepath.Join("testdata")
	doc, err := parseDocument([]byte(`
absent_parent:
  present_null: null
  disabled: false
  zero: 0
  empty: ""
  child: {}
  items: []
plugin_attr:
  prometheus:
    beyond_float64: 9007199254740993
    beyond_uint64: 184467440737095516160000000000000000000
`), pathBase, nil)
	if err != nil {
		t.Fatal(err)
	}

	if doc.kind != nodeMapping {
		t.Fatalf("document kind = %d, want mapping", doc.kind)
	}
	assertNodePathBase(t, doc, pathBase)
	if _, ok := doc.mapping["absent"]; ok {
		t.Fatal("absent path became present")
	}
	parent := doc.mapping["absent_parent"]
	if parent.mapping["present_null"].kind != nodeNull {
		t.Fatal("explicit null kind was lost")
	}
	if got := parent.mapping["disabled"].scalar; got != false {
		t.Fatalf("disabled = %#v, want false", got)
	}
	if got := parent.mapping["zero"].scalar; got != json.Number("0") {
		t.Fatalf("zero = %#v, want exact zero", got)
	}
	if got := parent.mapping["empty"].scalar; got != "" {
		t.Fatalf("empty = %#v, want empty string", got)
	}
	if got := parent.mapping["child"].kind; got != nodeMapping {
		t.Fatalf("child kind = %d, want mapping", got)
	}
	if got := parent.mapping["items"].kind; got != nodeSequence {
		t.Fatalf("items kind = %d, want sequence", got)
	}
	prometheus := doc.mapping["plugin_attr"].mapping["prometheus"]
	if got := prometheus.mapping["beyond_float64"].scalar; got != json.Number("9007199254740993") {
		t.Fatalf("beyond_float64 = %#v", got)
	}
	if got := prometheus.mapping["beyond_uint64"].scalar; got != json.Number(
		"184467440737095516160000000000000000000",
	) {
		t.Fatalf("beyond_uint64 = %#v", got)
	}

	converted := nodeToAny(doc).(map[string]any)
	convertedPrometheus := converted["plugin_attr"].(map[string]any)["prometheus"].(map[string]any)
	if got := convertedPrometheus["beyond_float64"]; got != json.Number("9007199254740993") {
		t.Fatalf("nodeToAny number = %#v, want json.Number", got)
	}
	convertedParent := converted["absent_parent"].(map[string]any)
	if value, ok := convertedParent["present_null"]; !ok || value != nil {
		t.Fatalf("nodeToAny explicit null = %#v/%t, want present nil", value, ok)
	}
}

func TestParseDocumentTreatsEmptyDocumentsAsEmptyMappings(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "empty", data: ""},
		{name: "comment only", data: "# local overrides are intentionally empty\n  # another comment\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := parseDocument([]byte(test.data), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if doc.kind != nodeMapping || len(doc.mapping) != 0 {
				t.Fatalf("empty document = %#v, want empty mapping", doc)
			}
		})
	}

	doc, err := parseDocument([]byte("null\n"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.kind != nodeNull {
		t.Fatalf("explicit null kind = %d, want nodeNull", doc.kind)
	}
}

func TestParseDocumentNormalizesYAMLNumbers(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want json.Number
	}{
		{name: "hex", raw: "0xFF", want: "255"},
		{name: "negative hex", raw: "-0x10", want: "-16"},
		{name: "explicit octal", raw: "0o17", want: "15"},
		{name: "binary", raw: "0b1010", want: "10"},
		{name: "legacy octal", raw: "077", want: "63"},
		{name: "underscores", raw: "1_000_000", want: "1000000"},
		{name: "leading plus", raw: "+42", want: "42"},
		{
			name: "high precision decimal",
			raw:  "1234567890.12345678901234567890",
			want: "1234567890.12345678901234567890",
		},
		{name: "missing integer part", raw: ".5", want: "0.5"},
		{name: "negative missing integer part", raw: "-.5", want: "-0.5"},
		{name: "missing fraction", raw: "1.", want: "1.0"},
		{name: "positive decimal", raw: "+1.25", want: "1.25"},
		{name: "decimal underscores", raw: "1_000.25_00", want: "1000.2500"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc, err := parseDocument([]byte("value: "+test.raw+"\n"), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := doc.mapping["value"].scalar; got != test.want {
				t.Fatalf("value = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseDocumentRejectsNonFiniteNumbersWithoutValueLeakage(t *testing.T) {
	for _, raw := range []string{
		".inf", ".Inf", ".INF", "+.inf", "+.Inf", "+.INF",
		"-.inf", "-.Inf", "-.INF", ".nan", ".NaN", ".NAN",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseDocument([]byte("safe_field: "+raw+"\n"), "", nil)
			if err == nil {
				t.Fatal("parseDocument() error = nil, want non-finite rejection")
			}
			if strings.Contains(err.Error(), raw) || !strings.Contains(err.Error(), "safe_field") {
				t.Fatalf("parseDocument() error = %q, want field path without raw number", err)
			}
		})
	}
}

func TestParseDocumentRejectsDuplicateMappingKeys(t *testing.T) {
	_, err := parseDocument([]byte("apisix:\n  enable_http2: true\n  enable_http2: false\n"),
		"", nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate key apisix.enable_http2") {
		t.Fatalf("parseDocument() error = %v", err)
	}
}

func TestParseDocumentRejectsExpandedMappingKeyCollisionsWithoutSecrets(t *testing.T) {
	const secret = "C2_SENTINEL_EXPANDED_KEY_49E62A"
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		vars []string
	}{
		{
			name: "template and template",
			yaml: "nodes:\n  '${{FIRST_KEY}}': 1\n  '${{SECOND_KEY}}': 2\n",
			env:  map[string]string{"FIRST_KEY": secret, "SECOND_KEY": secret},
			vars: []string{"FIRST_KEY", "SECOND_KEY"},
		},
		{
			name: "template and static",
			yaml: "nodes:\n  '${{KEY:= C2_SENTINEL_EXPANDED_KEY_49E62A }}': 1\n  C2_SENTINEL_EXPANDED_KEY_49E62A: 2\n",
			env:  map[string]string{},
			vars: []string{"KEY"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDocument([]byte(test.yaml), "", test.env)
			if err == nil {
				t.Fatal("parseDocument() error = nil, want expanded-key collision")
			}
			if !strings.Contains(err.Error(), "nodes") {
				t.Fatalf("parseDocument() error = %q, want parent path", err)
			}
			for _, name := range test.vars {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("parseDocument() error = %q, want variable %s", err, name)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("parseDocument() leaked expanded key or fallback: %q", err)
			}
		})
	}
}

func TestParseDocumentExpandsAPISIXEnvironmentInKeysAndValues(t *testing.T) {
	doc, err := parseDocument([]byte(`
deployment:
  etcd:
    host: ["http://${{ETCD_HOST}}:2379", "${{ETCD_HOST}}-${{ SUFFIX }}"]
plugin_attr:
  nodes:
    "${{ HOST }}:${{PORT:= 9080 }}": 1
trimmed_fallback: "${{OPTIONAL:=  fallback value  }}"
empty_fallback: "${{EMPTY_FALLBACK:=}}"
present_empty: "${{PRESENT_EMPTY:=not used}}"
`), "conf", map[string]string{
		"ETCD_HOST":     "etcd.internal",
		"SUFFIX":        "primary",
		"HOST":          "127.0.0.1",
		"PRESENT_EMPTY": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	hosts := doc.mapping["deployment"].mapping["etcd"].mapping["host"].sequence
	if got := hosts[0].scalar; got != "http://etcd.internal:2379" {
		t.Fatalf("first host = %#v", got)
	}
	if got := hosts[1].scalar; got != "etcd.internal-primary" {
		t.Fatalf("second host = %#v", got)
	}
	nodes := doc.mapping["plugin_attr"].mapping["nodes"]
	if _, ok := nodes.mapping["127.0.0.1:9080"]; !ok {
		t.Fatalf("expanded keys = %#v", nodes.mapping)
	}
	if got := doc.mapping["trimmed_fallback"].scalar; got != "fallback value" {
		t.Fatalf("trimmed_fallback = %#v", got)
	}
	if got := doc.mapping["empty_fallback"].scalar; got != "" {
		t.Fatalf("empty_fallback = %#v", got)
	}
	if got := doc.mapping["present_empty"].scalar; got != "" {
		t.Fatalf("present_empty = %#v", got)
	}
}

func TestParseDocumentUsesOnlySuppliedEnvironmentSnapshot(t *testing.T) {
	const name = "APISIX_GO_C2_AMBIENT_ONLY"
	t.Setenv(name, "ambient-secret")
	for _, env := range []map[string]string{nil, {}} {
		_, err := parseDocument([]byte("value: '${{"+name+"}}'\n"), "", env)
		want := "expand APISIX environment at value: " + name + " is not set"
		if err == nil || err.Error() != want {
			t.Fatalf("parseDocument() error = %v, want %q", err, want)
		}
		if strings.Contains(err.Error(), "ambient-secret") {
			t.Fatalf("parseDocument() leaked ambient value: %q", err)
		}
	}
}

func TestParseDocumentReportsFirstMissingAPISIXEnvironment(t *testing.T) {
	_, err := parseDocument([]byte("value: '${{FIRST_MISSING}}-${{SECOND_MISSING}}'\n"),
		"", map[string]string{})
	if err == nil || err.Error() != "expand APISIX environment at value: FIRST_MISSING is not set" {
		t.Fatalf("parseDocument() error = %v", err)
	}
}

func TestParseDocumentRejectsInvalidUTF8FromAPISIXEnvironment(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	for _, test := range []struct {
		data string
		want string
	}{
		{data: "'${{VALUE}}': safe\n", want: "expand APISIX environment at <root>: result is not valid UTF-8"},
		{data: "value: '${{VALUE}}'\n", want: "expand APISIX environment at value: result is not valid UTF-8"},
	} {
		_, err := parseDocument([]byte(test.data), "", map[string]string{
			"VALUE": invalidUTF8,
		})
		if err == nil || err.Error() != test.want {
			t.Fatalf("parseDocument() error = %v", err)
		}
		if strings.Contains(err.Error(), invalidUTF8) {
			t.Fatalf("parseDocument() leaked invalid value: %q", err)
		}
	}
}

func TestParseDocumentRetypesCompleteExpandedScalars(t *testing.T) {
	doc, err := parseDocument([]byte(`
quoted_true: "${{TRUE_VALUE}}"
quoted_false: '${{FALSE_VALUE}}'
large: "${{LARGE}}"
decimal: "${{DECIMAL}}"
prefix: "prefix-${{PORT}}"
legacy_leading_zero: "${{LEGACY_LEADING_ZERO}}"
leading_zero_eight: "${{LEADING_ZERO_EIGHT}}"
yaml_octal: "${{YAML_OCTAL}}"
yaml_binary: "${{YAML_BINARY}}"
positive_binary: "${{POSITIVE_BINARY}}"
negative_binary: "${{NEGATIVE_BINARY}}"
underscored: "${{UNDERSCORED}}"
positive_fraction: "${{POSITIVE_FRACTION}}"
exponent: "${{EXPONENT}}"
hex: "${{HEX}}"
positive_hex: "${{POSITIVE_HEX}}"
negative_hex: "${{NEGATIVE_HEX}}"
spaced_number: "${{SPACED_NUMBER}}"
`), "", map[string]string{
		"TRUE_VALUE":          "true",
		"FALSE_VALUE":         "false",
		"LARGE":               "184467440737095516160000000000000000000",
		"DECIMAL":             "-.5000000000000000000000001",
		"PORT":                "9080",
		"LEGACY_LEADING_ZERO": "077",
		"LEADING_ZERO_EIGHT":  "08",
		"YAML_OCTAL":          "0o17",
		"YAML_BINARY":         "0b10",
		"POSITIVE_BINARY":     "+0b10",
		"NEGATIVE_BINARY":     "-0b10",
		"UNDERSCORED":         "1_000",
		"POSITIVE_FRACTION":   "+1.25",
		"EXPONENT":            "+01.50e+03",
		"HEX":                 "0x10",
		"POSITIVE_HEX":        "+0x10",
		"NEGATIVE_HEX":        "-0x10",
		"SPACED_NUMBER":       "  42 \t",
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"quoted_true":         true,
		"quoted_false":        false,
		"large":               json.Number("184467440737095516160000000000000000000"),
		"decimal":             json.Number("-0.5000000000000000000000001"),
		"prefix":              "prefix-9080",
		"legacy_leading_zero": json.Number("77"),
		"leading_zero_eight":  json.Number("8"),
		"yaml_octal":          "0o17",
		"yaml_binary":         json.Number("2"),
		"positive_binary":     json.Number("2"),
		"negative_binary":     json.Number("-2"),
		"underscored":         "1_000",
		"positive_fraction":   json.Number("1.25"),
		"exponent":            json.Number("1.50e+03"),
		"hex":                 json.Number("16"),
		"positive_hex":        json.Number("16"),
		"negative_hex":        json.Number("-16"),
		"spaced_number":       json.Number("42"),
	} {
		if got := doc.mapping[key].scalar; got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
	}
}

func TestParseDocumentErrorsDoNotLeakSecretScalarContent(t *testing.T) {
	const secret = "C2_SENTINEL_SECRET_SCALAR_7B4D19"
	tests := []struct {
		name string
		yaml string
	}{
		{name: "anchored scalar", yaml: "safe: &value " + secret + "\n"},
		{name: "non string mapping key", yaml: "? [" + secret + "]\n: value\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDocument([]byte(test.yaml), "", nil)
			if err == nil {
				t.Fatal("parseDocument() error = nil")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("parseDocument() leaked secret scalar: %q", err)
			}
		})
	}
}

func TestParseDocumentRejectsMultipleDocuments(t *testing.T) {
	_, err := parseDocument([]byte("first: true\n---\nsecond: true\n"), "", nil)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("parseDocument() error = %v", err)
	}
}

func TestParseDocumentRejectsAnchorsAliasesAndMergeKeys(t *testing.T) {
	const secret = "C2_SENTINEL_YAML_REFERENCE_7CC751"
	tests := []struct {
		name     string
		yaml     string
		wantPath string
		wantKind string
	}{
		{name: "anchor", yaml: "root:\n  value: &shared " + secret + "\n", wantPath: "root.value", wantKind: "anchor"},
		{
			name:     "alias",
			yaml:     "base: &shared\n  value: " + secret + "\ncopy: *shared\n",
			wantPath: "copy", wantKind: "alias",
		},
		{
			name:     "merge",
			yaml:     "base: &shared\n  value: " + secret + "\ncombined:\n  <<: *shared\n",
			wantPath: "combined", wantKind: "merge",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseDocument([]byte(test.yaml), "", nil)
			if err == nil {
				t.Fatal("parseDocument() error = nil")
			}
			if !strings.Contains(err.Error(), test.wantPath) ||
				!strings.Contains(strings.ToLower(err.Error()), test.wantKind) {
				t.Fatalf("parseDocument() error = %q, want %s at %s", err, test.wantKind, test.wantPath)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("parseDocument() leaked referenced scalar: %q", err)
			}
		})
	}
}

func TestParseDocumentAllowsLiteralDoubleAngleMappingKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		yaml string
		want string
	}{
		{name: "quoted", yaml: "\"<<\": quoted\n", want: "quoted"},
		{name: "explicit string tag", yaml: "!!str <<: explicit\n", want: "explicit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, err := parseDocument([]byte(test.yaml), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := doc.mapping["<<"].scalar; got != test.want {
				t.Fatalf("literal << value = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestReadConfigDocumentWrapsReadAndParseFailures(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yaml")
	_, err := readConfigDocument(missing, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "read static configuration "+missing+": ") {
		t.Fatalf("readConfigDocument() read error = %v", err)
	}

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("first: true\n---\nsecond: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readConfigDocument(invalid, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "parse static configuration "+invalid+": ") {
		t.Fatalf("readConfigDocument() parse error = %v", err)
	}
}

func TestReadConfigDocumentDoesNotReadAmbientEnvironment(t *testing.T) {
	const name = "APISIX_GO_C2_READ_AMBIENT_ONLY"
	t.Setenv(name, "ambient-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("value: '${{"+name+"}}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readConfigDocument(path, map[string]string{})
	wantCause := "expand APISIX environment at value: " + name + " is not set"
	if err == nil || !strings.HasSuffix(err.Error(), wantCause) {
		t.Fatalf("readConfigDocument() error = %v, want suffix %q", err, wantCause)
	}
}

func TestNodeToAnyReturnsFreshMapsAndSlices(t *testing.T) {
	node := &valueNode{
		kind: nodeMapping,
		mapping: map[string]*valueNode{
			"items": {
				kind:     nodeSequence,
				sequence: []*valueNode{{kind: nodeScalar, scalar: json.Number("7")}},
			},
		},
	}
	first := nodeToAny(node).(map[string]any)
	second := nodeToAny(node).(map[string]any)
	first["items"].([]any)[0] = "changed"
	first["extra"] = true
	if got := second["items"].([]any)[0]; got != json.Number("7") {
		t.Fatalf("second conversion aliased first slice: %#v", got)
	}
	if _, ok := second["extra"]; ok {
		t.Fatal("second conversion aliased first map")
	}
	if got := nodeToAny(nil); got != nil {
		t.Fatalf("nodeToAny(nil) = %#v, want nil", got)
	}
}

func TestCloneNodeDeepCopiesState(t *testing.T) {
	if cloneNode(nil) != nil {
		t.Fatal("cloneNode(nil) != nil")
	}
	original := &valueNode{
		kind: nodeMapping,
		mapping: map[string]*valueNode{
			"items": {
				kind: nodeSequence,
				sequence: []*valueNode{{
					kind: nodeScalar, scalar: json.Number("9007199254740993"), pathBase: "/config",
				}},
			},
		},
		pathBase: "/config",
	}
	cloned := cloneNode(original)
	leaf := cloned.mapping["items"].sequence[0]
	if leaf.scalar != json.Number("9007199254740993") || leaf.pathBase != "/config" {
		t.Fatalf("clone lost leaf state: %#v", leaf)
	}
	cloned.mapping["items"].sequence[0].scalar = json.Number("1")
	cloned.mapping["new"] = &valueNode{kind: nodeNull}
	cloned.mapping["items"].sequence = append(cloned.mapping["items"].sequence, &valueNode{kind: nodeNull})
	if got := original.mapping["items"].sequence[0].scalar; got != json.Number("9007199254740993") {
		t.Fatalf("clone aliased scalar node: %#v", got)
	}
	if _, ok := original.mapping["new"]; ok {
		t.Fatal("clone aliased mapping")
	}
	if got := len(original.mapping["items"].sequence); got != 1 {
		t.Fatalf("clone aliased sequence: length %d", got)
	}
}

func assertNodePathBase(t *testing.T, node *valueNode, want string) {
	t.Helper()
	if node.pathBase != want {
		t.Fatalf("node pathBase = %q, want %q", node.pathBase, want)
	}
	for _, child := range node.mapping {
		assertNodePathBase(t, child, want)
	}
	for _, child := range node.sequence {
		assertNodePathBase(t, child, want)
	}
}
