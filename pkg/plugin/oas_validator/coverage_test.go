package oas_validator

import (
	"bytes"
	"mime/multipart"
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeYAMLValue(t *testing.T) {
	value, err := normalizeYAMLValue(map[any]any{"name": "alice", "nested": map[any]any{"age": 3}})
	if err != nil {
		t.Fatalf("normalizeYAMLValue() error = %v", err)
	}
	typed := value.(map[string]any)
	if typed["name"] != "alice" || typed["nested"].(map[string]any)["age"] != 3 {
		t.Fatalf("normalized = %#v", typed)
	}

	items, err := normalizeYAMLValue([]any{"a", "b"})
	if err != nil || len(items.([]any)) != 2 {
		t.Fatalf("normalizeYAMLValue(list) = %#v/%v", items, err)
	}

	if _, err := normalizeYAMLValue(map[any]any{3: "x"}); err == nil {
		t.Fatal("normalizeYAMLValue(non-string key) error = nil")
	}
}

func TestParseYAMLBody(t *testing.T) {
	value, err := parseYAMLBody([]byte("name: alice\ncount: 3\n"))
	if err != nil {
		t.Fatalf("parseYAMLBody() error = %v", err)
	}
	typed := value.(map[string]any)
	if typed["name"] != "alice" || typed["count"] != 3 {
		t.Fatalf("parsed = %#v", typed)
	}
	if _, err := parseYAMLBody([]byte(": : :")); err == nil {
		t.Fatal("parseYAMLBody(invalid) error = nil")
	}
}

func TestFormBodyValue(t *testing.T) {
	values := url.Values{"name": {"alice"}, "tags": {"a", "b"}}
	schema := map[string]any{
		"properties": map[string]any{"count": map[string]any{"type": "integer"}},
	}
	data := formBodyValue(values, schema)
	if data["name"] != "alice" {
		t.Fatalf("name = %#v", data["name"])
	}
	if data["count"] = 0; data["count"] != 0 {
		t.Fatal("unexpected count")
	}
	tags, ok := data["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("tags = %#v", data["tags"])
	}
}

func TestCoerceValueBySchemaType(t *testing.T) {
	schema := map[string]any{"type": "integer"}
	if got := coerceValue("7", schema); got != int64(7) {
		t.Fatalf("coerceValue(integer) = %#v, want 7", got)
	}
	schema = map[string]any{"type": "number"}
	if got := coerceValue("3.5", schema); got != 3.5 {
		t.Fatalf("coerceValue(number) = %#v, want 3.5", got)
	}
	schema = map[string]any{"type": "boolean"}
	if got := coerceValue("true", schema); got != true {
		t.Fatalf("coerceValue(boolean) = %#v, want true", got)
	}
	schema = map[string]any{"type": "string"}
	if got := coerceValue("7", schema); got != "7" {
		t.Fatalf("coerceValue(string) = %#v, want passthrough", got)
	}
}

func TestAdditionalPropertySchema(t *testing.T) {
	if _, ok := additionalPropertySchema(map[string]any{}); ok {
		t.Fatal("additionalPropertySchema(absent) = ok, want absent")
	}
	if schema, ok := additionalPropertySchema(
		map[string]any{"additionalProperties": map[string]any{"type": "string"}},
	); !ok ||
		schema["type"] != "string" {
		t.Fatalf("additionalPropertySchema(map) = %#v/%t", schema, ok)
	}
	if _, ok := additionalPropertySchema(map[string]any{"additionalProperties": true}); !ok {
		t.Fatal("additionalPropertySchema(true) = not ok")
	}
	if _, ok := additionalPropertySchema(map[string]any{"additionalProperties": 3}); ok {
		t.Fatal("additionalPropertySchema(other) = ok, want absent")
	}
}

func TestMultipartBodyValue(t *testing.T) {
	if _, err := multipartBodyValue([]byte("part"), "", nil); err == nil {
		t.Fatal("multipartBodyValue(no boundary) error = nil")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("name", "alice")
	_ = writer.Close()

	values, err := multipartBodyValue(body.Bytes(), writer.Boundary(), nil)
	if err != nil {
		t.Fatalf("multipartBodyValue() error = %v", err)
	}
	if values["name"] != "alice" {
		t.Fatalf("multipart values = %#v", values)
	}

	if _, err := multipartBodyValue([]byte("not multipart"), "boundary", nil); err == nil {
		t.Fatal("multipartBodyValue(invalid) error = nil")
	}
}

func TestBodyDecoderForFamily(t *testing.T) {
	tests := []struct {
		family string
		want   string
	}{
		{family: "application/json", want: "json"},
		{family: "application/x-www-form-urlencoded", want: "form"},
		{family: "multipart/form-data", want: "multipart"},
		{family: "text/plain", want: "text"},
		{family: "application/octet-stream", want: "octet"},
		{family: "unknown/type", want: "unknown"},
	}
	for _, test := range tests {
		decoder := bodyDecoderForFamily(test.family)
		if decoder == nil {
			t.Fatalf("bodyDecoderForFamily(%q) = nil", test.family)
		}
	}
}

func TestBodyDecoders(t *testing.T) {
	jsonValue, err := jsonBodyDecoder(strings.NewReader(`{"count": 2}`), nil, nil, nil)
	if err != nil || jsonValue.(map[string]any)["count"] != float64(2) {
		t.Fatalf("jsonBodyDecoder() = %#v/%v", jsonValue, err)
	}
	if _, err := jsonBodyDecoder(strings.NewReader("{bad"), nil, nil, nil); err == nil {
		t.Fatal("jsonBodyDecoder(invalid) error = nil")
	}

	textValue, err := stringBodyDecoder(strings.NewReader("raw"), nil, nil, nil)
	if err != nil || textValue != "raw" {
		t.Fatalf("stringBodyDecoder() = %#v/%v", textValue, err)
	}

	formValue, err := formBodyDecoder(strings.NewReader("name=alice"), nil, nil, nil)
	if err != nil || formValue.(map[string]any)["name"] != "alice" {
		t.Fatalf("formBodyDecoder() = %#v/%v", formValue, err)
	}
	if _, err := formBodyDecoder(strings.NewReader("%zz"), nil, nil, nil); err == nil {
		t.Fatal("formBodyDecoder(invalid) error = nil")
	}

	yamlValue, err := yamlBodyDecoder(strings.NewReader("name: alice\n"), nil, nil, nil)
	if err != nil || yamlValue.(map[string]any)["name"] != "alice" {
		t.Fatalf("yamlBodyDecoder() = %#v/%v", yamlValue, err)
	}
}
