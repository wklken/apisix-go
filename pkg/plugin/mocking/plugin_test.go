package mocking

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestSchemaValidatesResponseStatusHTTPBounds(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, test := range []struct {
		name    string
		value   any
		wantErr bool
	}{
		{name: "informational", value: 100, wantErr: true},
		{name: "early informational", value: 103, wantErr: true},
		{name: "last informational", value: 199, wantErr: true},
		{name: "minimum", value: 200},
		{name: "standard maximum", value: 599},
		{name: "extended three digit minimum", value: 600},
		{name: "three digit maximum", value: 999},
		{name: "four digit", value: 1000, wantErr: true},
		{name: "fractional", value: 200.5, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"response_example": "ok",
				"response_status":  test.value,
			}
			err := util.Validate(config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatalf("response_status=%v should fail schema validation", test.value)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("response_status=%v should validate: %v", test.value, err)
			}
		})
	}
}

func TestHandlerSendsExtendedThreeDigitStatusOnWire(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "600", status: 600},
		{name: "999", status: 999},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			example := "ok"
			p := newTestPlugin(t, Config{ResponseStatus: status, ResponseExample: &example})
			server := httptest.NewServer(p.Handler(http.NotFoundHandler()))
			t.Cleanup(server.Close)

			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatalf("GET status %d: %v", status, err)
			}

			if response.StatusCode != status {
				t.Fatalf("status = %d, want %d", response.StatusCode, status)
			}
			body, err := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if err != nil {
				t.Fatalf("read response body: %v", err)
			}
			if closeErr != nil {
				t.Fatalf("close response body: %v", closeErr)
			}
			if got := string(body); got != example {
				t.Fatalf("body = %q, want %q", got, example)
			}
		})
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

func TestGenerateByPropertyMatchesAPISIX317RandomDefaultBounds(t *testing.T) {
	for range 100 {
		stringValue, ok := generateByProperty(map[string]any{"type": "string"}).(string)
		if !ok || len(stringValue) < 1 || len(stringValue) > 10 ||
			strings.Trim(stringValue, "abcdefghijklmnopqrstuvwxyz") != "" {
			t.Fatalf("generated string = %#v, want 1..10 lowercase ASCII characters", stringValue)
		}

		numberValue, ok := generateByProperty(map[string]any{"type": "number"}).(float64)
		if !ok || numberValue < 0 || numberValue >= 10000 {
			t.Fatalf("generated number = %#v, want value in [0, 10000)", numberValue)
		}

		integerValue, ok := generateByProperty(map[string]any{"type": "integer"}).(float64)
		if !ok || integerValue < 1 || integerValue > 10000 || math.Trunc(integerValue) != integerValue {
			t.Fatalf("generated integer = %#v, want integer in [1, 10000]", integerValue)
		}

		if _, ok := generateByProperty(map[string]any{"type": "boolean"}).(bool); !ok {
			t.Fatal("generated boolean has the wrong type")
		}

		arrayValue, ok := generateByProperty(map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		}).([]any)
		if !ok || len(arrayValue) < 1 || len(arrayValue) > 3 {
			t.Fatalf("generated array = %#v, want 1..3 items", arrayValue)
		}
		for _, item := range arrayValue {
			value, ok := item.(string)
			if !ok || value == "" {
				t.Fatalf("generated array item = %#v, want non-empty string", item)
			}
		}
	}
}

func TestPostInitRejectsAPISIX317UnsupportedContentType(t *testing.T) {
	example := "ok"
	p := &Plugin{config: Config{
		ContentType:     "application/octet-stream",
		ResponseExample: &example,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want unsupported content type rejected")
	}
}

func TestHandlerMatchesAPISIX317EmptyTextHTMLSchemaBody(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message": map[string]any{"type": "string", "example": "not emitted"},
		},
	}
	p := newTestPlugin(t, Config{ContentType: "text/html", ResponseSchema: &schema})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mocking should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/mock", nil))

	if got := rr.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty APISIX 3.17 text/html schema body", got)
	}
}

func TestHandlerMatchesAPISIX317TextPlainCharset(t *testing.T) {
	example := "hello world"
	p := newTestPlugin(t, Config{ContentType: "text/plain", ResponseExample: &example})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("mocking should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/hello", nil))

	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 text/plain response charset", got)
	}
	if got := rr.Body.String(); got != "hello world" {
		t.Fatalf("body = %q, want hello world", got)
	}
}

func TestRunRequestPhasePublishesEarlyStopSource(t *testing.T) {
	example := `{"ok":true}`
	p := newTestPlugin(t, Config{ResponseExample: &example})
	request := httptest.NewRequest(http.MethodGet, "/mock", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("result = %+v, want early-stop stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("source = %q, want early_stop", lifecycle.ResponseSource())
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
