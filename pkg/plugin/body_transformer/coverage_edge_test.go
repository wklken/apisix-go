package body_transformer

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDetectFormatCoversContentTypesAndExplicitOverride(t *testing.T) {
	plugin := &Plugin{}
	tests := []struct {
		name        string
		transform   *Transform
		contentType string
		method      string
		want        string
	}{
		{name: "GET always args", transform: &Transform{InputFormat: "json"}, method: http.MethodGet, want: "args"},
		{name: "explicit", transform: &Transform{InputFormat: "yaml"}, contentType: "application/json", want: "yaml"},
		{name: "JSON", transform: &Transform{}, contentType: "APPLICATION/JSON; charset=utf-8", want: "json"},
		{name: "encoded", transform: &Transform{}, contentType: "application/x-www-form-urlencoded", want: "encoded"},
		{name: "XML", transform: &Transform{}, contentType: "text/xml", want: "xml"},
		{name: "multipart", transform: &Transform{}, contentType: "multipart/form-data; boundary=x", want: "multipart"},
		{name: "plain", transform: &Transform{}, contentType: "text/plain", want: "plain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := plugin.detectFormat(test.transform, test.contentType, test.method); got != test.want {
				t.Fatalf("detectFormat() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildTemplateContextHandlesEmptyInvalidAndReservedValues(t *testing.T) {
	plugin := &Plugin{}
	req := httptest.NewRequest(http.MethodPost, "http://example.test/?tag=a&tag=b", nil)
	for _, format := range []string{"json", "yaml", "xml", "plain", ""} {
		ctx, err := plugin.buildTemplateContext(req, nil, format, "request", "")
		if err != nil || len(ctx.values) != 0 {
			t.Fatalf("empty %s context = %#v, %v", format, ctx.values, err)
		}
	}

	for _, test := range []struct {
		format      string
		body        string
		contentType string
	}{
		{format: "json", body: `{`},
		{format: "yaml", body: "items: ["},
		{format: "encoded", body: "a=1;b=2"},
		{format: "xml", body: "<root>"},
		{format: "multipart", body: "body", contentType: "text/plain"},
	} {
		if _, err := plugin.buildTemplateContext(
			req,
			[]byte(test.body),
			test.format,
			"request",
			test.contentType,
		); err == nil {
			t.Fatalf("buildTemplateContext(%s invalid body) error = nil", test.format)
		}
	}

	ctx, err := plugin.buildTemplateContext(
		req,
		[]byte(`{"_ctx":"spoofed","_body":"spoofed","value":"safe"}`),
		"json",
		"request",
		"application/json",
	)
	if err != nil {
		t.Fatalf("buildTemplateContext(valid JSON) error = %v", err)
	}
	if _, ok := ctx.values["_ctx"]; ok {
		t.Fatal("reserved _ctx value remained available")
	}
	if _, ok := ctx.values["_body"]; ok {
		t.Fatal("reserved _body value remained available")
	}
	if ctx.values["value"] != "safe" {
		t.Fatalf("safe template value = %q", ctx.values["value"])
	}
}

func TestRenderTemplateLazyCompilationAndBase64Selection(t *testing.T) {
	ctx := templateContext{values: map[string]string{"name": "alice"}, structured: map[string]any{}, format: "json"}
	encoded := base64.StdEncoding.EncodeToString([]byte(`hello {{ name }}`))
	transform := &Transform{Template: encoded}
	got, err := renderTemplate(transform, ctx)
	if err != nil || got != "hello alice" {
		t.Fatalf("renderTemplate(base64) = %q, %v", got, err)
	}

	plain := &Transform{Template: `hello {{ name }}`}
	got, err = renderTemplate(plain, templateContext{
		values: map[string]string{"name": "alice"}, structured: map[string]any{}, format: "encoded",
	})
	if err != nil || got != "hello alice" {
		t.Fatalf("renderTemplate(lazy plain) = %q, %v", got, err)
	}
}

func TestTemplateValidationRejectsMalformedControlFlow(t *testing.T) {
	invalidTemplates := []string{
		`value *}`,
		`{* *}`,
		`value }}`,
		`{{ }}`,
		`{% if true %}missing then{% end %}`,
		`{% if true then %}missing end`,
		`{% if true then %}a{% else %}b{% else %}c{% end %}`,
		`{% if true then %}a{% else %}b{% elseif false then %}c{% end %}`,
		`{% if true then %}a{% unknown %}{% end %}`,
		`{% if true then %}a{%`,
	}
	for _, input := range invalidTemplates {
		compiled := compileTemplate(input)
		if _, err := compiled.render(templateContext{}); err == nil {
			t.Fatalf("compileTemplate(%q) rendered without error", input)
		}
	}

	for _, test := range []struct {
		directive string
		keyword   string
	}{
		{directive: "else", keyword: "if"},
		{directive: "if true", keyword: "if"},
		{directive: "if then", keyword: "if"},
	} {
		if _, err := parseTemplateConditionDirective(test.directive, test.keyword); err == nil {
			t.Fatalf("parseTemplateConditionDirective(%q) error = nil", test.directive)
		}
	}
}

func TestTemplateSplittersRespectQuotesEscapesAndNesting(t *testing.T) {
	if got := splitTemplateKeyword(
		`name == "a or b" or (count > 1 and count < 3)`,
		"or",
	); !reflect.DeepEqual(
		got,
		[]string{`name == "a or b" `, ` (count > 1 and count < 3)`},
	) {
		t.Fatalf("splitTemplateKeyword() = %#v", got)
	}
	if got := splitTemplateArguments(`value, "a,b", nested(one, two), 'escaped\'quote'`); len(got) != 4 {
		t.Fatalf("splitTemplateArguments() = %#v", got)
	}
	if got := splitTemplateOperator(`"a+b" + nested(1+2) + 'c\'d'`, "+"); len(got) != 3 {
		t.Fatalf("splitTemplateOperator() = %#v", got)
	}
}

func TestSingleQuotedTemplateLiteralEscapes(t *testing.T) {
	tests := map[string]string{
		`'a\ab'`: "a\ab",
		`'a\bb'`: "a\bb",
		`'a\fb'`: "a\fb",
		`'a\nb'`: "a\nb",
		`'a\rb'`: "a\rb",
		`'a\tb'`: "a\tb",
		`'a\vb'`: "a\vb",
		`'a\\b'`: `a\b`,
		`'a\'b'`: "a'b",
		`'a\"b'`: `a"b`,
	}
	for input, want := range tests {
		got, ok := templateStringLiteral(input)
		if !ok || got != want {
			t.Fatalf("templateStringLiteral(%q) = %q, %t; want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{`'a\z'`, `'a\'`} {
		if _, ok := templateStringLiteral(input); ok {
			t.Fatalf("templateStringLiteral(%q) accepted invalid escape", input)
		}
	}
}

func TestTemplatePathFlatteningAndRepeatedValues(t *testing.T) {
	for input, want := range map[string]string{
		`items[0]["name"]`: "items.0.name",
		`["root"]`:         "root",
		`items[`:           `items[`,
		`items[]`:          `items[]`,
		`plain`:            `plain`,
	} {
		if got := normalizeTemplatePath(input); got != want {
			t.Fatalf("normalizeTemplatePath(%q) = %q, want %q", input, got, want)
		}
	}

	values := map[string]string{}
	flattenValues("", map[string]any{
		"name":  "alice",
		"items": []any{float64(1), true, nil, struct{ Value string }{Value: "x"}},
	}, values)
	for key, want := range map[string]string{
		"name": "alice", "items.0": "1", "items.1": "true", "items.2": "null", "items.3": "{x}",
	} {
		if values[key] != want {
			t.Fatalf("flattenValues()[%q] = %q, want %q; all=%#v", key, values[key], want, values)
		}
	}

	repeated := map[string]string{}
	setRepeatedValues(repeated, "empty", nil)
	setRepeatedValues(repeated, "tag", []string{"a", "b"})
	if _, ok := repeated["empty"]; ok || repeated["tag"] != "a" || repeated["tag.1"] != "b" {
		t.Fatalf("setRepeatedValues() = %#v", repeated)
	}
}

func TestMultipartFlatteningRejectsMissingOrEmptyParts(t *testing.T) {
	for _, test := range []struct {
		body        string
		contentType string
	}{
		{body: "body", contentType: "not a media type;"},
		{body: "body", contentType: "text/plain"},
		{body: "body", contentType: "multipart/form-data"},
		{body: "not-a-part", contentType: "multipart/form-data; boundary=test"},
	} {
		if err := flattenMultipartValues([]byte(test.body), test.contentType, map[string]string{}); err == nil {
			t.Fatalf("flattenMultipartValues(%q) error = nil", test.contentType)
		}
	}
}

func TestConfigReturnsMutablePluginConfiguration(t *testing.T) {
	plugin := &Plugin{}
	if got := plugin.Config(); got != &plugin.config {
		t.Fatalf("Config() = %p, want %p", got, &plugin.config)
	}
}

func TestResolveExpressionCoversStructuredAndArithmeticFallbacks(t *testing.T) {
	ctx := templateContext{
		values:     map[string]string{"name": "alice", "count": "2"},
		structured: map[string]any{"items": []any{"a", "b"}},
		body:       "raw",
		req:        httptest.NewRequest(http.MethodGet, "http://example.test/?id=42", nil),
	}
	for expr, want := range map[string]string{
		`string.gsub(name, "a", "A")`: "Alice",
		`string.gsub(name, "a")`:      "",
		`_escape_json(items)`:         `["a","b"]`,
		`_escape_xml("<tag>")`:        "&lt;tag&gt;",
		`"a" .. name`:                 "aalice",
		`count + 3`:                   "5",
		`name + 3`:                    "",
		`_body`:                       "raw",
		`_ctx.var.arg_id`:             "42",
	} {
		if got := resolveExpression(expr, ctx); got != want {
			t.Fatalf("resolveExpression(%q) = %q, want %q", expr, got, want)
		}
	}
	if value, ok := structuredTemplateValue(
		`items[0]`,
		templateContext{structured: map[string]any{"items.0": "a"}},
	); !ok ||
		value != "a" {
		t.Fatalf("structuredTemplateValue() = %#v, %t", value, ok)
	}
}

func TestResetBufferedResponseHandlesUnwrappingCycle(t *testing.T) {
	writer := &selfUnwrappingWriter{ResponseRecorder: httptest.NewRecorder()}
	resetBufferedResponse(writer)
}

type selfUnwrappingWriter struct {
	*httptest.ResponseRecorder
}

func (w *selfUnwrappingWriter) Unwrap() http.ResponseWriter {
	return w
}

func TestErrorReaderPropagatesThroughRequestTransform(t *testing.T) {
	plugin := &Plugin{config: Config{Request: &Transform{Template: "body"}}}
	_ = plugin.PostInit()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
	req.Body = errorReadCloser{err: io.ErrUnexpectedEOF}
	if _, err := plugin.transformRequest(req); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("transformRequest() error = %v", err)
	}
}

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r errorReadCloser) Close() error             { return nil }
