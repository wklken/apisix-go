package util

import (
	"encoding/json"
	"reflect"
	"testing"
)

type parseParityTarget struct {
	Count    int               `json:"count"`
	Label    string            `json:"label"`
	Enabled  bool              `json:"enabled"`
	Ratio    float64           `json:"ratio"`
	Tags     []string          `json:"tags"`
	Meta     map[string]string `json:"meta"`
	Optional *string           `json:"optional,omitempty"`
	Any      any               `json:"any"`
	Nested   struct {
		Deep int64 `json:"deep"`
	} `json:"nested"`
}

type parseFallbackTarget struct {
	Count int `json:"count"`
	Embedded
}

type Embedded struct {
	Value string `json:"value"`
}

type parseCustomUnmarshalerTarget struct {
	Count json.Number `json:"count"`
}

func (t *parseCustomUnmarshalerTarget) UnmarshalJSON(data []byte) error {
	var raw struct {
		Count json.Number `json:"count"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Count = raw.Count
	return nil
}

func parseParitySources() map[string]any {
	return map[string]any{
		"count":    2,
		"label":    "hello",
		"enabled":  true,
		"ratio":    0.5,
		"tags":     []any{"a", "b"},
		"meta":     map[string]any{"k": "v"},
		"optional": nil,
		"any":      map[string]any{"x": 3},
		"nested":   map[string]any{"deep": 7},
	}
}

func TestParseWalkerMatchesRoundtrip(t *testing.T) {
	source := parseParitySources()

	var walker parseParityTarget
	if err := Parse(source, &walker); err != nil {
		t.Fatalf("Parse(walker) error = %v", err)
	}

	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip parseParityTarget
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal(roundtrip) error = %v", err)
	}
	if !reflect.DeepEqual(walker, roundtrip) {
		t.Fatalf("walker = %#v, roundtrip = %#v", walker, roundtrip)
	}
}

func TestParseWalkerMatchesRoundtripForEveryScalarForm(t *testing.T) {
	sources := []map[string]any{
		{"count": 0},
		{"count": 2.0},
		{"count": int64(3)},
		{"count": json.Number("4")},
		{"count": "not-a-number"},
		{"count": 1.5},
		{"count": nil},
		{"label": "x", "ratio": 1e100},
	}
	for _, source := range sources {
		var walker parseParityTarget
		walkerErr := Parse(source, &walker)
		encoded, _ := json.Marshal(source)
		var roundtrip parseParityTarget
		roundtripErr := json.Unmarshal(encoded, &roundtrip)
		if (walkerErr == nil) != (roundtripErr == nil) {
			t.Fatalf("source %v: walker error = %v, roundtrip error = %v", source, walkerErr, roundtripErr)
		}
		if walkerErr == nil && !reflect.DeepEqual(walker, roundtrip) {
			t.Fatalf("source %v: walker = %#v, roundtrip = %#v", source, walker, roundtrip)
		}
	}
}

func TestParseWalkerMatchesRoundtripForInterfaceNumbers(t *testing.T) {
	sources := []map[string]any{
		{"any": int64(5)},
		{"any": json.Number("6")},
		{"any": 7.5},
		{"any": []any{int64(1), json.Number("2"), "x"}},
		{"any": map[string]any{"n": int64(8), "s": "v"}},
	}
	for _, source := range sources {
		var walker parseParityTarget
		if err := Parse(source, &walker); err != nil {
			t.Fatalf("Parse(walker) error = %v", err)
		}
		encoded, _ := json.Marshal(source)
		var roundtrip parseParityTarget
		if err := json.Unmarshal(encoded, &roundtrip); err != nil {
			t.Fatalf("json.Unmarshal error = %v", err)
		}
		if !reflect.DeepEqual(walker.Any, roundtrip.Any) {
			t.Fatalf("source %v: walker any = %#v (%T), roundtrip any = %#v (%T)",
				source, walker.Any, walker.Any, roundtrip.Any, roundtrip.Any)
		}
	}
}

func TestParseWalkerMatchesRoundtripForCaseInsensitiveFields(t *testing.T) {
	source := map[string]any{"COUNT": 9, "Label": "upper"}
	var walker parseParityTarget
	if err := Parse(source, &walker); err != nil {
		t.Fatalf("Parse(walker) error = %v", err)
	}
	if walker.Count != 9 || walker.Label != "upper" {
		t.Fatalf("walker = %#v, want case-insensitive match", walker)
	}
}

func TestParseWalkerMatchesRoundtripForNull(t *testing.T) {
	source := map[string]any{"tags": nil, "meta": nil, "optional": nil}
	var walker parseParityTarget
	if err := Parse(source, &walker); err != nil {
		t.Fatalf("Parse(walker) error = %v", err)
	}
	encoded, _ := json.Marshal(source)
	var roundtrip parseParityTarget
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatalf("json.Unmarshal error = %v", err)
	}
	if !reflect.DeepEqual(walker, roundtrip) {
		t.Fatalf("walker = %#v, roundtrip = %#v", walker, roundtrip)
	}
}

func TestParseFallsBackForEmbeddedFields(t *testing.T) {
	source := map[string]any{"count": 4, "value": "embedded"}
	var target parseFallbackTarget
	if err := Parse(source, &target); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if target.Count != 4 || target.Value != "embedded" {
		t.Fatalf("target = %#v, want embedded field populated", target)
	}
}

func TestParseFallsBackForCustomUnmarshaler(t *testing.T) {
	source := map[string]any{"count": 7}
	var target parseCustomUnmarshalerTarget
	if err := Parse(source, &target); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if target.Count.String() != "7" {
		t.Fatalf("count = %q, want 7", target.Count.String())
	}
}

func TestParsePointerDestinationRequired(t *testing.T) {
	if err := Parse(map[string]any{}, parseParityTarget{}); err == nil {
		t.Fatal("Parse(non-pointer dest) error = nil")
	}
	var nilPointer *parseParityTarget
	if err := Parse(map[string]any{}, nilPointer); err == nil {
		t.Fatal("Parse(nil pointer dest) error = nil")
	}
}
