package json_test

import (
	"bytes"
	stdjson "encoding/json"
	"strings"
	"testing"

	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

type hookValue struct {
	Value string
}

func (v *hookValue) UnmarshalJSON(data []byte) error {
	var raw struct {
		Value string `json:"value"`
	}
	if err := stdjson.Unmarshal(data, &raw); err != nil {
		return err
	}
	v.Value = "hook:" + raw.Value
	return nil
}

func TestMarshalMatchesStandardLibrary(t *testing.T) {
	input := map[string]any{
		"html":  "<script>&",
		"nil":   []string(nil),
		"value": "雪",
	}
	want, err := stdjson.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := apisixjson.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Marshal() = %q, want %q", got, want)
	}
}

func TestRawMessageAndCustomUnmarshalHook(t *testing.T) {
	var envelope struct {
		Raw   apisixjson.RawMessage `json:"raw"`
		Value hookValue             `json:"value"`
	}
	if err := apisixjson.Unmarshal(
		[]byte(`{"raw":{"n":1},"value":{"value":"ok"}}`),
		&envelope,
	); err != nil {
		t.Fatal(err)
	}
	if string(envelope.Raw) != `{"n":1}` {
		t.Fatalf("RawMessage = %s", envelope.Raw)
	}
	if envelope.Value.Value != "hook:ok" {
		t.Fatalf("custom hook value = %q", envelope.Value.Value)
	}
}

func TestDecoderUseNumberPreservesLargeInteger(t *testing.T) {
	decoder := apisixjson.NewDecoder(strings.NewReader(`{"id":9007199254740993}`))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	number, ok := value["id"].(apisixjson.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Fatalf("decoded id = %#v", value["id"])
	}
}

func TestEncoderMatchesStandardLibraryDefaults(t *testing.T) {
	input := map[string]string{"html": "<tag>"}
	var got bytes.Buffer
	encoder := apisixjson.NewEncoder(&got)
	if err := encoder.Encode(input); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := stdjson.NewEncoder(&want).Encode(input); err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() {
		t.Fatalf("Encode() = %q, want %q", got.String(), want.String())
	}
}

func TestUnmarshalRejectsTrailingDataLikeStandardLibrary(t *testing.T) {
	input := []byte(`{"ok":true} trailing`)
	var got, want map[string]any
	gotErr := apisixjson.Unmarshal(input, &got)
	wantErr := stdjson.Unmarshal(input, &want)
	if gotErr == nil || wantErr == nil {
		t.Fatalf("Unmarshal() error = %v, stdlib = %v; trailing data must be rejected", gotErr, wantErr)
	}
}
