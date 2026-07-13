package mocking

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaAcceptsResponseSchema(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"response_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string", "example": "ok"},
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("mocking response_schema config should validate: %v", err)
	}
}

func TestHandlerGeneratesJSONFromResponseSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "example": "ok"},
			"count":   map[string]any{"type": "integer", "example": float64(7)},
			"enabled": map[string]any{"type": "boolean", "example": true},
			"nested": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "example": "inner"},
				},
			},
		},
	}
	p := newTestPlugin(t, Config{ResponseSchema: &schema})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mocking should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mock", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Content-Type"); got != defaultContentType {
		t.Fatalf("content-type = %q, want %q", got, defaultContentType)
	}
	if got := rr.Header().Get("x-mock-by"); got == "" {
		t.Fatal("x-mock-by header should be set")
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body should be json: %v, body=%q", err, rr.Body.String())
	}
	if got := body["message"]; got != "ok" {
		t.Fatalf("message = %#v, want ok", got)
	}
	if got := body["count"]; got != float64(7) {
		t.Fatalf("count = %#v, want 7", got)
	}
	if got := body["enabled"]; got != true {
		t.Fatalf("enabled = %#v, want true", got)
	}
	nested, ok := body["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %#v, want object", body["nested"])
	}
	if got := nested["name"]; got != "inner" {
		t.Fatalf("nested.name = %#v, want inner", got)
	}
}

func TestHandlerPrefersResponseExampleOverSchemaAndResolvesVariables(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "example": "schema"},
		},
	}
	example := `{"uri":"$uri","query":"$arg_name"}`
	p := newTestPlugin(t, Config{
		ResponseExample: &example,
		ResponseSchema:  &schema,
		ResponseHeaders: map[string]any{
			"X-Mock-URI": "$uri",
			"X-Count":    float64(3),
		},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mocking should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mock?name=alice", nil))

	if got := rr.Body.String(); got != `{"uri":"/mock","query":"alice"}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("X-Mock-URI"); got != "/mock" {
		t.Fatalf("X-Mock-URI = %q, want /mock", got)
	}
	if got := rr.Header().Get("X-Count"); got != "3" {
		t.Fatalf("X-Count = %q, want 3", got)
	}
}

func TestHandlerGeneratesXMLFromResponseSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "example": "ok"},
		},
	}
	p := newTestPlugin(t, Config{ContentType: "application/xml", ResponseSchema: &schema})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mocking should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mock", nil))

	if got := rr.Body.String(); got != "<data><message>ok</message></data>" {
		t.Fatalf("body = %q", got)
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
