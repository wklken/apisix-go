package config

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestMergeNodesRecursivelyMergesMappingsAndReplacesOtherKinds(t *testing.T) {
	lower := mustNodeFromAny(map[string]any{
		"section": map[string]any{
			"keep": "lower", "replace": "lower", "list": []string{"lower"},
		},
	}, "/defaults")
	upper := mustNodeFromAny(map[string]any{
		"section": map[string]any{
			"replace": "upper", "list": []string{"upper"}, "empty_map": map[string]any{},
		},
	}, "/overrides")

	merged := mergeNodes(lower, upper)
	section := merged.mapping["section"]
	if got := section.mapping["keep"].scalar; got != "lower" {
		t.Fatalf("kept value = %#v", got)
	}
	if got := section.mapping["replace"].scalar; got != "upper" {
		t.Fatalf("replacement value = %#v", got)
	}
	if got := section.mapping["list"].sequence[0].scalar; got != "upper" {
		t.Fatalf("replacement list = %#v", got)
	}
	if got := section.mapping["empty_map"]; got.kind != nodeMapping || len(got.mapping) != 0 {
		t.Fatalf("empty map = %#v", got)
	}
	if section.pathBase != "/overrides" || section.mapping["keep"].pathBase != "/defaults" ||
		section.mapping["replace"].pathBase != "/overrides" {
		t.Fatalf("merged path bases are wrong: %#v", section)
	}
}

func TestMergeNodesDoesNotAliasInputsOrResults(t *testing.T) {
	lower := mustNodeFromAny(map[string]any{"mapping": map[string]any{"lower": "value"}}, "/lower")
	upper := mustNodeFromAny(map[string]any{"mapping": map[string]any{"upper": []string{"value"}}}, "/upper")
	merged := mergeNodes(lower, upper)

	merged.mapping["mapping"].mapping["lower"].scalar = "changed"
	merged.mapping["mapping"].mapping["upper"].sequence[0].scalar = "changed"
	if lower.mapping["mapping"].mapping["lower"].scalar != "value" ||
		upper.mapping["mapping"].mapping["upper"].sequence[0].scalar != "value" {
		t.Fatal("merged result aliases an input")
	}
	upper.mapping["mapping"].mapping["upper"].sequence[0].scalar = "later"
	if got := merged.mapping["mapping"].mapping["upper"].sequence[0].scalar; got != "changed" {
		t.Fatalf("later input mutation leaked into result: %#v", got)
	}
}

func TestAppendConfigPathKeyDistinguishesLiteralAndNestedKeys(t *testing.T) {
	for key, want := range map[string]string{
		"safe-key": "safe-key",
		"tenant.a": `["tenant.a"]`,
		"items[0]": `["items[0]"]`,
		"":         `[""]`,
	} {
		if got := appendConfigPathKey("", key); got != want {
			t.Fatalf("appendConfigPathKey(%q) = %q, want %q", key, got, want)
		}
	}
	if got := appendConfigPathKey("root", "child"); got != "root.child" {
		t.Fatalf("nested path = %q", got)
	}
}

func TestNodeFromAnyPreservesSupportedValuesAndPathBase(t *testing.T) {
	tests := []struct {
		name string
		in   any
		kind nodeKind
		want any
	}{
		{name: "nil", in: nil, kind: nodeNull, want: nil},
		{name: "bool", in: true, kind: nodeScalar, want: true},
		{name: "string", in: "value", kind: nodeScalar, want: "value"},
		{name: "int64", in: int64(math.MinInt64), kind: nodeScalar, want: json.Number("-9223372036854775808")},
		{name: "uint64", in: uint64(math.MaxUint64), kind: nodeScalar, want: json.Number("18446744073709551615")},
		{name: "number", in: json.Number("9007199254740993"), kind: nodeScalar, want: json.Number("9007199254740993")},
		{name: "map", in: map[string]any{"enabled": false}, kind: nodeMapping, want: map[string]any{"enabled": false}},
		{name: "slice", in: []string{"a", "b"}, kind: nodeSequence, want: []any{"a", "b"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node, err := nodeFromAny(test.in, "/config")
			if err != nil {
				t.Fatal(err)
			}
			if node.kind != test.kind || node.pathBase != "/config" || !reflect.DeepEqual(nodeToAny(node), test.want) {
				t.Fatalf("node = %#v, value = %#v", node, nodeToAny(node))
			}
		})
	}
}

func TestNodeFromAnyDeepCopiesNestedInput(t *testing.T) {
	input := map[string]any{"nested": map[string]any{"items": []string{"original"}}}
	node, err := nodeFromAny(input, "")
	if err != nil {
		t.Fatal(err)
	}
	input["nested"].(map[string]any)["items"].([]string)[0] = "changed"
	if got := node.mapping["nested"].mapping["items"].sequence[0].scalar; got != "original" {
		t.Fatalf("input mutation leaked into node: %#v", got)
	}
}

func TestNodeFromAnyRejectsUnsupportedOrInvalidValuesWithoutLeakingThem(t *testing.T) {
	const secret = "must-not-appear"
	for _, test := range []struct {
		name string
		in   any
	}{
		{name: "float", in: 1.5},
		{name: "invalid number", in: json.Number(secret)},
		{name: "non-string map key", in: map[int]string{1: secret}},
		{name: "function", in: func() {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node, err := nodeFromAny(test.in, "")
			if err == nil || node != nil {
				t.Fatalf("nodeFromAny() = %#v, %v", node, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("nodeFromAny() leaked value: %q", err)
			}
		})
	}
}

func TestMustNodeFromAnyPanicsForInvalidBuiltin(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("mustNodeFromAny() did not panic")
		}
	}()
	mustNodeFromAny(1.5, "")
}
