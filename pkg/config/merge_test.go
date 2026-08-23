package config

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestMergeEveryKindPairUsesExplicitPrecedence(t *testing.T) {
	lowerSource := FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"}
	upperSource := FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}
	nodes := map[string]*valueNode{
		"null":     {kind: nodeNull},
		"scalar":   {kind: nodeScalar, scalar: "upper"},
		"mapping":  {kind: nodeMapping, mapping: map[string]*valueNode{"upper": {kind: nodeScalar, scalar: "yes"}}},
		"sequence": {kind: nodeSequence, sequence: []*valueNode{{kind: nodeScalar, scalar: "item"}}},
	}
	for lowerName, lowerTemplate := range nodes {
		for upperName, upperTemplate := range nodes {
			t.Run(lowerName+"_then_"+upperName, func(t *testing.T) {
				lower := cloneNode(lowerTemplate)
				lower.source = lowerSource
				if lower.kind == nodeMapping {
					lower.mapping["lower"] = &valueNode{kind: nodeScalar, scalar: "kept", source: lowerSource}
				}
				upper := cloneNode(upperTemplate)
				upper.source = upperSource
				merged := mergeNodes(lower, upper)

				if lower.kind == nodeMapping && upper.kind == nodeMapping {
					if merged.kind != nodeMapping || merged.mapping["lower"].scalar != "kept" ||
						merged.mapping["upper"].scalar != "yes" {
						t.Fatalf("mapping merge = %#v", nodeToAny(merged))
					}
					return
				}
				if !reflect.DeepEqual(nodeToAny(merged), nodeToAny(upper)) || merged.kind != upper.kind {
					t.Fatalf(
						"merged = %#v/%d, want upper %#v/%d",
						nodeToAny(merged), merged.kind, nodeToAny(upper), upper.kind,
					)
				}
			})
		}
	}

	lower := &valueNode{kind: nodeScalar, scalar: "lower", source: lowerSource}
	upper := &valueNode{kind: nodeScalar, scalar: "upper", source: upperSource}
	if got := mergeNodes(lower, nil); got == lower || got.scalar != "lower" {
		t.Fatalf("mergeNodes(lower, nil) = %#v", got)
	}
	if got := mergeNodes(nil, upper); got == upper || got.scalar != "upper" {
		t.Fatalf("mergeNodes(nil, upper) = %#v", got)
	}
	if got := mergeNodes(nil, nil); got != nil {
		t.Fatalf("mergeNodes(nil, nil) = %#v", got)
	}
}

func TestMergePresenceEmptyContainersAndWinningMetadata(t *testing.T) {
	lowerSource := FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"}
	upperSource := FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}
	lower := mustNodeFromAny(map[string]any{
		"section": map[string]any{
			"keep": "lower", "replace": "lower", "false": true, "zero": 1,
			"list": []any{"a", "b"}, "nullable": "lower", "empty_map": map[string]any{"child": "kept"},
		},
	}, lowerSource)
	setNodePathBase(lower, "/defaults")
	upper := mustNodeFromAny(map[string]any{
		"section": map[string]any{
			"replace": "", "false": false, "zero": 0, "list": []any{}, "nullable": nil,
			"empty_map": map[string]any{},
		},
	}, upperSource)
	setNodePathBase(upper, "/overrides")

	merged := mergeNodes(lower, upper)
	section := merged.mapping["section"]
	if section.source != upperSource || section.pathBase != "/overrides" {
		t.Fatalf("section metadata = %+v/%q", section.source, section.pathBase)
	}
	if section.mapping["keep"].scalar != "lower" || section.mapping["keep"].source != lowerSource ||
		section.mapping["keep"].pathBase != "/defaults" {
		t.Fatalf("lower-only child = %#v", section.mapping["keep"])
	}
	for key, want := range map[string]any{"replace": "", "false": false, "zero": json.Number("0")} {
		if got := section.mapping[key].scalar; got != want {
			t.Fatalf("%s = %#v, want %#v", key, got, want)
		}
		if section.mapping[key].source != upperSource || section.mapping[key].pathBase != "/overrides" {
			t.Fatalf("%s metadata = %#v", key, section.mapping[key])
		}
	}
	if section.mapping["nullable"].kind != nodeNull {
		t.Fatal("explicit null did not replace")
	}
	if list := section.mapping["list"]; list.kind != nodeSequence || len(list.sequence) != 0 {
		t.Fatalf("empty sequence = %#v", list)
	}
	if emptyMap := section.mapping["empty_map"]; emptyMap.mapping["child"].scalar != "kept" ||
		emptyMap.source != upperSource || emptyMap.pathBase != "/overrides" {
		t.Fatalf("empty upper mapping = %#v", emptyMap)
	}

	envSource := FieldSource{Kind: SourceAPISIXEnv, Origin: "FILE_VALUE", Explicit: true}
	envNode := &valueNode{kind: nodeScalar, scalar: "expanded", source: envSource, pathBase: "/from-file"}
	envMerged := mergeNodes(nil, envNode)
	if envMerged.source != envSource || envMerged.pathBase != "/from-file" {
		t.Fatalf("APISIX source/path base = %+v/%q", envMerged.source, envMerged.pathBase)
	}
}

func TestMergeDeeplyDetachesInputsAndResult(t *testing.T) {
	lowerSource := FieldSource{Kind: SourceDefaultFile, Origin: "default.yaml"}
	upperSource := FieldSource{Kind: SourceOverrideFile, Origin: "override.yaml", Explicit: true}
	lower := mustNodeFromAny(map[string]any{
		"mapping":  map[string]any{"lower": "value"},
		"sequence": []any{map[string]any{"lower": "item"}},
	}, lowerSource)
	upper := mustNodeFromAny(map[string]any{
		"mapping": map[string]any{"upper": "value"},
	}, upperSource)
	setNodePathBase(lower, "/lower")
	setNodePathBase(upper, "/upper")
	lowerBefore := cloneNode(lower)
	upperBefore := cloneNode(upper)

	merged := mergeNodes(lower, upper)
	merged.mapping["mapping"].mapping["lower"].scalar = "changed-result"
	merged.mapping["sequence"].sequence[0].mapping["lower"].scalar = "changed-result"
	merged.mapping["mapping"].mapping["new"] = &valueNode{kind: nodeScalar, scalar: "new"}
	merged.mapping["sequence"].sequence = append(merged.mapping["sequence"].sequence, &valueNode{kind: nodeNull})
	merged.mapping["mapping"].source.Origin = "changed-result"
	merged.mapping["mapping"].pathBase = "/changed-result"
	if !reflect.DeepEqual(lower, lowerBefore) || !reflect.DeepEqual(upper, upperBefore) {
		t.Fatalf("result mutation changed inputs\nlower=%#v\nupper=%#v", lower, upper)
	}

	merged = mergeNodes(lower, upper)
	lower.mapping["mapping"].mapping["lower"].scalar = "changed-input"
	lower.mapping["sequence"].sequence[0].mapping["lower"].scalar = "changed-input"
	upper.mapping["mapping"].mapping["upper"].scalar = "changed-input"
	upper.mapping["mapping"].source.Origin = "changed-input"
	upper.mapping["mapping"].pathBase = "/changed-input"
	if got := merged.mapping["mapping"].mapping["lower"].scalar; got != "value" {
		t.Fatalf("later lower mutation leaked: %#v", got)
	}
	if got := merged.mapping["mapping"].mapping["upper"].scalar; got != "value" {
		t.Fatalf("later upper mutation leaked: %#v", got)
	}
	if got := merged.mapping["sequence"].sequence[0].mapping["lower"].scalar; got != "item" {
		t.Fatalf("later sequence mutation leaked: %#v", got)
	}
	if got := merged.mapping["mapping"].source.Origin; got != "override.yaml" {
		t.Fatalf("later source mutation leaked: %q", got)
	}
	if got := merged.mapping["mapping"].pathBase; got != "/upper" {
		t.Fatalf("later pathBase mutation leaked: %q", got)
	}
}

func TestFlattenProvenanceRecordsEveryNodeWithCanonicalPaths(t *testing.T) {
	source := FieldSource{Kind: SourceBuiltin, Origin: "fixture"}
	root := &valueNode{kind: nodeMapping, source: source, mapping: map[string]*valueNode{
		"safe-key":   {kind: nodeScalar, scalar: "value", source: source},
		"empty_map":  {kind: nodeMapping, mapping: map[string]*valueNode{}, source: source},
		"empty_list": {kind: nodeSequence, sequence: []*valueNode{}, source: source},
		"items": {kind: nodeSequence, source: source, sequence: []*valueNode{
			{kind: nodeScalar, scalar: "scalar", source: source},
			{kind: nodeMapping, source: source, mapping: map[string]*valueNode{
				"child": {kind: nodeScalar, scalar: true, source: source},
			}},
			{kind: nodeSequence, source: source, sequence: []*valueNode{
				{kind: nodeNull, source: source},
			}},
		}},
		"tenant.a": {kind: nodeScalar, scalar: "literal-dot", source: source},
		"tenant": {kind: nodeMapping, source: source, mapping: map[string]*valueNode{
			"a": {kind: nodeScalar, scalar: "nested-dot", source: source},
		}},
		"nested": {kind: nodeMapping, source: source, mapping: map[string]*valueNode{
			"0": {kind: nodeScalar, scalar: "leading-digit", source: source},
		}},
		"items[0]":    {kind: nodeScalar, scalar: "literal-index", source: source},
		"quote\"":     {kind: nodeScalar, scalar: "quote", source: source},
		"back\\slash": {kind: nodeScalar, scalar: "backslash", source: source},
		"control\x01": {kind: nodeScalar, scalar: "control", source: source},
		"":            {kind: nodeScalar, scalar: "empty", source: source},
	}}

	got := flattenProvenance(root)
	wantPaths := []string{
		"safe-key", "empty_map", "empty_list", "items", "items[0]", "items[1]", "items[1].child",
		"items[2]", "items[2][0]", `["tenant.a"]`, "nested", `nested["0"]`, `["items[0]"]`,
		`["quote\""]`, `["back\\slash"]`, `["control\u0001"]`, `[""]`, "tenant", "tenant.a",
	}
	if len(got) != len(wantPaths) {
		t.Fatalf("provenance paths = %#v, want %d entries", got, len(wantPaths))
	}
	for _, path := range wantPaths {
		if got[path] != source {
			t.Errorf("provenance[%q] = %+v, want %+v", path, got[path], source)
		}
	}
	if _, exists := got[""]; exists {
		t.Fatal("root provenance was emitted")
	}
	if got[`["items[0]"]`] != source || got["items[0]"] != source ||
		got[`["tenant.a"]`] != source || got["tenant.a"] != source {
		t.Fatalf("unsafe literal paths collided: %#v", got)
	}
	const controlPath = `["control\u0001"]`
	var decodedControlKey string
	if err := json.Unmarshal([]byte(controlPath[1:len(controlPath)-1]), &decodedControlKey); err != nil {
		t.Fatalf("canonical control key is not JSON: %v", err)
	}
	if decodedControlKey != "control\x01" {
		t.Fatalf("decoded control key = %q", decodedControlKey)
	}
}

func TestNodeFromAnyAcceptsExactSupportedValues(t *testing.T) {
	type namedBool bool
	type namedString string
	type namedInt int32
	type namedUint uint64
	type namedKey string
	source := FieldSource{Kind: SourceCLI, Origin: "test", Explicit: true}
	tests := []struct {
		name string
		in   any
		kind nodeKind
		want any
	}{
		{name: "nil", in: nil, kind: nodeNull},
		{name: "typed nil map", in: map[string]any(nil), kind: nodeNull},
		{name: "typed nil slice", in: []string(nil), kind: nodeNull},
		{name: "bool", in: true, kind: nodeScalar, want: true},
		{name: "named bool", in: namedBool(true), kind: nodeScalar, want: true},
		{name: "string", in: "value", kind: nodeScalar, want: "value"},
		{name: "named string", in: namedString("value"), kind: nodeScalar, want: "value"},
		{name: "json number", in: json.Number("-12.50e+2"), kind: nodeScalar, want: json.Number("-12.50e+2")},
		{name: "int", in: int(-1), kind: nodeScalar, want: json.Number("-1")},
		{name: "int8", in: int8(-8), kind: nodeScalar, want: json.Number("-8")},
		{name: "int16", in: int16(-16), kind: nodeScalar, want: json.Number("-16")},
		{name: "int32", in: int32(-32), kind: nodeScalar, want: json.Number("-32")},
		{name: "int64", in: int64(math.MinInt64), kind: nodeScalar, want: json.Number("-9223372036854775808")},
		{name: "named int", in: namedInt(-32), kind: nodeScalar, want: json.Number("-32")},
		{name: "uint", in: uint(1), kind: nodeScalar, want: json.Number("1")},
		{name: "uint8", in: uint8(8), kind: nodeScalar, want: json.Number("8")},
		{name: "uint16", in: uint16(16), kind: nodeScalar, want: json.Number("16")},
		{name: "uint32", in: uint32(32), kind: nodeScalar, want: json.Number("32")},
		{name: "uint64", in: uint64(math.MaxUint64), kind: nodeScalar, want: json.Number("18446744073709551615")},
		{name: "uintptr", in: uintptr(math.MaxUint64), kind: nodeScalar, want: json.Number("18446744073709551615")},
		{
			name: "named uint", in: namedUint(math.MaxUint64),
			kind: nodeScalar, want: json.Number("18446744073709551615"),
		},
		{
			name: "named map key", in: map[namedKey]int{"key": 3},
			kind: nodeMapping, want: map[string]any{"key": json.Number("3")},
		},
		{name: "slice", in: []any{"x", 2}, kind: nodeSequence, want: []any{"x", json.Number("2")}},
		{name: "array", in: [2]bool{true, false}, kind: nodeSequence, want: []any{true, false}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, err := nodeFromAny(test.in, source)
			if err != nil {
				t.Fatal(err)
			}
			if node.kind != test.kind || node.source != source || !reflect.DeepEqual(nodeToAny(node), test.want) {
				t.Fatalf(
					"node = %#v/%d/%+v, want %#v/%d/%+v",
					nodeToAny(node), node.kind, node.source, test.want, test.kind, source,
				)
			}
		})
	}
}

func TestNodeFromAnyDeepCopiesMapsSlicesAndArrays(t *testing.T) {
	source := FieldSource{Kind: SourceCLI, Origin: "plugin_attr", Explicit: true}
	inputMap := map[string]any{"nested": map[string]string{"key": "value"}, "slice": []string{"a"}}
	node, err := nodeFromAny(inputMap, source)
	if err != nil {
		t.Fatal(err)
	}
	inputMap["nested"].(map[string]string)["key"] = "changed"
	inputMap["slice"].([]string)[0] = "changed"
	if got := node.mapping["nested"].mapping["key"].scalar; got != "value" {
		t.Fatalf("map mutation leaked: %#v", got)
	}
	if got := node.mapping["slice"].sequence[0].scalar; got != "a" {
		t.Fatalf("slice mutation leaked: %#v", got)
	}
	node.mapping["nested"].mapping["key"].scalar = "node-changed"
	if got := inputMap["nested"].(map[string]string)["key"]; got != "changed" {
		t.Fatalf("node mutation leaked to map: %q", got)
	}
}

func TestNodeFromAnyRejectsUnsupportedValuesWithoutLeakage(t *testing.T) {
	const secret = "C3_NODE_SENTINEL_7E34"
	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name string
		in   any
	}{
		{name: "malformed number", in: json.Number(secret)},
		{name: "leading-zero number", in: json.Number("01")},
		{name: "float32", in: float32(1.5)},
		{name: "float64", in: 1.5},
		{name: "pointer", in: func() *string { value := secret; return &value }()},
		{name: "struct", in: struct{ Value string }{Value: secret}},
		{name: "non-string map key", in: map[int]string{1: secret}},
		{name: "invalid UTF-8 string", in: invalidUTF8},
		{name: "invalid UTF-8 map key", in: map[string]any{invalidUTF8: secret, "�": secret}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, err := nodeFromAny(test.in, FieldSource{Kind: SourceCLI})
			if err == nil || node != nil {
				t.Fatalf("nodeFromAny() = %#v, %v", node, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "1.5") {
				t.Fatalf("nodeFromAny() leaked value: %q", err)
			}
		})
	}
}

func TestMustNodeFromAnyPanicsOnlyForInvalidBuiltin(t *testing.T) {
	node := mustNodeFromAny(map[string]any{"enabled": false}, FieldSource{Kind: SourceBuiltin, Origin: "defaults"})
	if got := node.mapping["enabled"].scalar; got != false {
		t.Fatalf("valid builtin = %#v", got)
	}
	defer func() {
		recovered := recover()
		if recovered == nil || !strings.Contains(recovered.(string), "invalid builtin configuration") {
			t.Fatalf("panic = %#v", recovered)
		}
	}()
	mustNodeFromAny(1.5, FieldSource{Kind: SourceBuiltin, Origin: "defaults"})
}

func setNodePathBase(node *valueNode, pathBase string) {
	if node == nil {
		return
	}
	node.pathBase = pathBase
	for _, child := range node.mapping {
		setNodePathBase(child, pathBase)
	}
	for _, child := range node.sequence {
		setNodePathBase(child, pathBase)
	}
}
