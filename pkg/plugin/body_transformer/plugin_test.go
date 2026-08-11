package body_transformer

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/client_control"
)

func TestBodyTransformerDescriptorSeparatesRequestAndResponse(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		wantStage  string
		wantHeader bool
		wantBody   bool
	}{
		{
			name:      "request only",
			config:    Config{Request: &Transform{Template: "request"}},
			wantStage: "rewrite",
		},
		{
			name:      "response only",
			config:    Config{Response: &Transform{Template: "response"}},
			wantStage: "none",
			wantBody:  true,
		},
		{
			name:      "request and response",
			config:    Config{Request: &Transform{Template: "request"}, Response: &Transform{Template: "response"}},
			wantStage: "rewrite",
			wantBody:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := newTestPlugin(t, tt.config)
			descriptor, err := plugin.Config().(base.BindingPhaseDescriber).DescribeBindingPhases()
			if err != nil {
				t.Fatalf("DescribeBindingPhases() error = %v", err)
			}
			if descriptor.RequestStage != tt.wantStage || descriptor.Header != tt.wantHeader ||
				descriptor.BufferedBody != tt.wantBody {
				t.Fatalf(
					"descriptor = %+v, want stage=%q header=%t body=%t",
					descriptor,
					tt.wantStage,
					tt.wantHeader,
					tt.wantBody,
				)
			}
		})
	}
}

func TestBodyTransformerRunsRequestAndResponsePhasesSeparately(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		Request:  &Transform{InputFormat: "plain", Template: "rewritten-request"},
		Response: &Transform{InputFormat: "plain", Template: "rewritten-response"},
	})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/anything", strings.NewReader("request"))
	req.Header.Set("Content-Type", "text/plain")
	requestResult := plugin.RunRequestPhase(httptest.NewRecorder(), req)
	if requestResult.Decision != base.RequestContinue || requestResult.Request == nil {
		t.Fatalf("RunRequestPhase() = %+v, want continue with request", requestResult)
	}
	state := base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/plain"}},
		Body:   []byte("response"),
	}
	if err := plugin.RunBufferedBodyFilter(requestResult.Request, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if got := string(state.Body); got != "rewritten-response" {
		t.Fatalf("response body = %q, want rewritten-response", got)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerTransformsJSONRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"full_name":"{{name}}","raw":{*_escape_json(_body)*}}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"full_name":"alice","raw":"{\"name\":\"alice\"}"}` {
			t.Fatalf("transformed body = %q", body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerTransformsYAMLRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "yaml",
			Template:    `{"foobar":"{{foobar.foo .. " " .. foobar.bar}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader("foobar:\n  foo: hello\n  bar: world\n"),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"foobar":"hello world"}` {
			t.Fatalf("transformed body = %q, want YAML values", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesBoundedTemplateExpressions(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"foo":"{{name .. " world"}}","bar":{{age+10}}}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"hello","age":20}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"foo":"hello world","bar":30}` {
			t.Fatalf("transformed body = %q, want bounded expression result", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesBoundedTemplateIfElse(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{% if enabled == true then %}{"status":"on"}{% else %}{"status":"off"}{% end %}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"status":"on"}` {
			t.Fatalf("transformed body = %q, want conditional branch", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesBoundedTemplateNilCondition(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{% if missing == nil then %}missing{% else %}present{% end %}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != "missing" {
			t.Fatalf("transformed body = %q, want nil branch", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesBoundedTemplateElseIf(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{% if state == "new" then %}new{% elseif state == "ready" then %}ready{% else %}other{% end %}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"state":"ready"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != "ready" {
			t.Fatalf("transformed body = %q, want elseif branch", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesRawTemplateExpression(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "plain",
			Template:    `{* _body *}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("raw-body"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != "raw-body" {
			t.Fatalf("transformed body = %q, want raw body", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerEvaluatesRawExpressionNextToJSONClosingBrace(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"foobar":{*_escape_json(name)*}}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"safe"}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"foobar":"safe"}` {
			t.Fatalf("transformed body = %q, want raw JSON expression", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%q", rr.Code, rr.Body.String())
	}
}

func TestHandlerDistinguishesEscapedAndRawTemplateExpressions(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{
			name:     "escaped",
			template: `{"agent":"{{_ctx.var.http_user_agent}}"}`,
			want:     `{"agent":"agent&#47;1.0"}`,
		},
		{
			name:     "raw",
			template: `{"agent":"{*_ctx.var.http_user_agent*}"}`,
			want:     `{"agent":"agent/1.0"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Request: &Transform{InputFormat: "plain", Template: test.template}})
			req := httptest.NewRequest(http.MethodPost, "/anything", nil)
			req.Header.Set("User-Agent", "agent/1.0")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read transformed body: %v", err)
				}
				if string(body) != test.want {
					t.Fatalf("transformed body = %q, want %q", body, test.want)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestHandlerSupportsNestedConditionalBranches(t *testing.T) {
	p := newTestPlugin(t, Config{Request: &Transform{
		InputFormat: "json",
		Template: `{% if outer then %}{% if first then %}A{% elseif second then %}B{% else %}C{% end %}` +
			`{% else %}D{% end %}`,
	}})
	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"outer":true,"first":false,"second":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != "B" {
			t.Fatalf("transformed body = %q, want B", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%q", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsUnsupportedTemplateDirective(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "plain",
			Template:    `{% for item in items do %}{{item}}{% end %}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("raw-body"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for unsupported template directive")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unsupported template directive") {
		t.Fatalf("body = %q, want unsupported-directive error", rr.Body.String())
	}
}

func TestHandlerReturnsServiceUnavailableForUnsupportedTemplateFunction(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"foo":"{{name() .. " world"}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"hello"}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for an unsupported template function")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "attempt to call global 'name'") {
		t.Fatalf("body = %q, want unsupported function error", rr.Body.String())
	}
}

func TestResolveBoundedExpressions(t *testing.T) {
	ctx := templateContext{values: map[string]string{"name": "hello", "age": "20"}}
	if got := resolveExpression(`name .. " world"`, ctx); got != "hello world" {
		t.Fatalf("string expression = %q, want hello world", got)
	}
	if got := resolveExpression(`name .. ' again'`, ctx); got != "hello again" {
		t.Fatalf("single-quoted string expression = %q, want hello again", got)
	}
	if got := resolveExpression("age+10", ctx); got != "30" {
		t.Fatalf("numeric expression = %q, want 30", got)
	}
}

func TestHandlerSupportsBoundedStringGsub(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "plain",
			Template:    `{"message":"{* string.gsub(_body, 'not ', '') *}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("not actually json"))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"message":"actually json"}` {
			t.Fatalf("transformed body = %q, want bounded string.gsub result", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestEvaluateTemplateConditionSupportsFalseAndContextValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/anything?name=alice", nil)
	ctx := templateContext{
		values: map[string]string{"enabled": "false"},
		req:    req,
	}
	tests := []struct {
		expression string
		want       bool
	}{
		{expression: "enabled", want: false},
		{expression: "enabled == false", want: true},
		{expression: `_ctx.var.arg_name == "alice"`, want: true},
	}
	for _, test := range tests {
		t.Run(test.expression, func(t *testing.T) {
			if got := evaluateTemplateCondition(test.expression, ctx); got != test.want {
				t.Fatalf("evaluateTemplateCondition(%q) = %t, want %t", test.expression, got, test.want)
			}
		})
	}
}

func TestHandlerResolvesNestedJSONAndArrayValues(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"name":"{{user.name}}","second":"{{items.1}}","bracket":"{{items[1]}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"user":{"name":"alice"},"items":["first","second"]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"alice","second":"second","bracket":"second"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestBuildTemplateContextReservesTemplateHelpers(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	body := []byte(
		`{"_ctx":"shadow","_body":"shadow","_escape_json":"shadow","_escape_xml":"shadow","_multipart":"shadow"}`,
	)

	ctx, err := p.buildTemplateContext(req, body, "json", "request", "application/json")
	if err != nil {
		t.Fatalf("buildTemplateContext() error = %v", err)
	}
	for _, reserved := range []string{"_ctx", "_body", "_escape_json", "_escape_xml", "_multipart"} {
		if _, ok := ctx.values[reserved]; ok {
			t.Fatalf("reserved template helper %q was exposed as a body value", reserved)
		}
	}
	if got := resolveExpression("_body", ctx); got != string(body) {
		t.Fatalf("_body = %q, want original body", got)
	}
}

func TestHandlerTransformsXMLRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			Template: `{"name":"{{user.name}}","city":"{{user.address.city}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`<user><name>alice</name><address><city>shenzhen</city></address></user>`),
	)
	req.Header.Set("Content-Type", "text/xml")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"alice","city":"shenzhen"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsBodyWithoutXMLElementWhenXMLIsRequired(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "xml",
			Template:    `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"alice"}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for non-XML input")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body decode") {
		t.Fatalf("body = %q, want XML decode error", rr.Body.String())
	}
}

func TestHandlerTransformsRepeatedXMLValuesWithIndexes(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "xml",
			Template:    `{"first":"{{root.user.0.name}}","second":"{{root.user.1.name}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`<root><user><name>alice</name></user><user><name>bob</name></user></root>`),
	)
	req.Header.Set("Content-Type", "text/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"first":"alice","second":"bob"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerSerializesNamespacedXMLSubtreeAsJSON(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "xml",
			Template:    `{*_escape_json(Envelope.Body)*}`,
		},
	})

	xmlBody := `<env:Envelope xmlns:env="urn:env" xmlns:ns="urn:test"><env:Body><ns:Resp>` +
		`<ns:orderId>v1</ns:orderId><ns:items><ns:item><ns:sku>first</ns:sku></ns:item>` +
		`<ns:item><ns:sku>second</ns:sku></ns:item></ns:items></ns:Resp></env:Body></env:Envelope>`
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(xmlBody))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		var transformed map[string]any
		if err := json.Unmarshal(body, &transformed); err != nil {
			t.Fatalf("unmarshal transformed body %q: %v", body, err)
		}
		response := transformed["Resp"].(map[string]any)
		if response["orderId"] != "v1" {
			t.Fatalf("orderId = %v, want v1", response["orderId"])
		}
		items := response["items"].(map[string]any)["item"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["sku"] != "first" ||
			items[1].(map[string]any)["sku"] != "second" {
			t.Fatalf("items = %#v, want first and second", items)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%q", rr.Code, rr.Body.String())
	}
}

func TestHandlerTransformsXMLAttributes(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "xml",
			Template:    `{"type":"{{root.item._attr.type}}","id":"{{root.item._attr.id}}"}`,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`<root><item ns:type="natural" id="42" xmlns:ns="urn:test">Alice</item></root>`),
	)
	req.Header.Set("Content-Type", "text/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"type":"natural","id":"42"}` {
			t.Fatalf("transformed body = %q, want XML attributes", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerUsesArgsForGETRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			Template: `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=bob", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"bob"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerResolvesRepeatedArgsAndEncodedValuesByIndex(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
	}{
		{
			name:   "args",
			method: http.MethodGet,
			path:   "/anything?tag=first&tag=second",
		},
		{
			name:        "encoded",
			method:      http.MethodPost,
			path:        "/anything",
			body:        "tag=first&tag=second",
			contentType: "application/x-www-form-urlencoded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Request: &Transform{
					InputFormat: test.name,
					Template:    `{"first":"{{tag}}","second":"{{tag.1}}"}`,
				},
			})
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				transformed, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read transformed body: %v", err)
				}
				if string(transformed) != `{"first":"first","second":"second"}` {
					t.Fatalf("transformed body = %q, want indexed repeated values", transformed)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestHandlerTransformsMultipartRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "multipart",
			Template:    `{"name":"{{name}}","roles":"{{roles.0}}/{{roles.1}}"}`,
		},
	})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "alice"); err != nil {
		t.Fatalf("write name field: %v", err)
	}
	if err := writer.WriteField("roles", "admin"); err != nil {
		t.Fatalf("write first role field: %v", err)
	}
	if err := writer.WriteField("roles", "viewer"); err != nil {
		t.Fatalf("write second role field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/anything", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transformed, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(transformed) != `{"name":"alice","roles":"admin/viewer"}` {
			t.Fatalf("transformed body = %q", transformed)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsMalformedMultipartRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "multipart",
			Template:    `{"name":"{{name}}"}`,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body decode") {
		t.Fatalf("body = %q, want multipart decode error", rr.Body.String())
	}
}

func TestHandlerSupportsBase64TemplateAndCtxVars(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			Template:         base64.StdEncoding.EncodeToString([]byte(`{"name":"{{_ctx.var.arg_name}}"}`)),
			TemplateIsBase64: true,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=carol", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"carol"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerResolvesRegisteredContextVariables(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "plain",
			Template:    `{"status":"{{_ctx.var.status}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$status", http.StatusAccepted)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"status":"202"}` {
			t.Fatalf("transformed body = %q, want registered status variable", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerDecodesBase64JSONTemplateWithoutExplicitFlag(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    base64.StdEncoding.EncodeToString([]byte(`{"name":"{{name}}"}`)),
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"alice"}` {
			t.Fatalf("transformed body = %q, want decoded base64 template", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerTransformsResponseBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Response: &Transform{
			InputFormat: "json",
			Template:    `{"result":"{{message}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != `{"result":"ok"}` {
		t.Fatalf("response body = %q, want transformed result", rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != "" {
		t.Fatalf("Content-Length = %q, want empty after rewrite", rr.Header().Get("Content-Length"))
	}
}

func TestHandlerResponseReplacementInvalidatesRepresentationHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		Response: &Transform{
			InputFormat: "plain",
			Template:    "rewritten",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, field := range responseRepresentationHeaders() {
			w.Header().Set(field, "stale")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})).ServeHTTP(rr, req)

	for _, field := range responseRepresentationHeaders() {
		if values := rr.Header().Values(field); len(values) != 0 {
			t.Errorf("%s = %v, want removed after response replacement", field, values)
		}
	}
}

func responseRepresentationHeaders() []string {
	return []string{
		"Content-Length", "Content-Encoding", "Content-Range", "Content-MD5",
		"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	}
}

func TestResponseTransformErrorReplacesSharedPipelineBody(t *testing.T) {
	p := newTestPlugin(t, Config{Response: &Transform{
		InputFormat: "json",
		Template:    `{"result":"{{message}}"}`,
	}})
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`not-json`))
	})
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := base.GetOrCreateTransformResponseWriter(r)
		p.Handler(downstream).ServeHTTP(recorder, r)
		recorder.Commit(w)
	})
	handler := base.WithTransformPipeline(2)(outer)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "not-json") {
		t.Fatalf("response body leaked upstream body: %q", rr.Body.String())
	}
}

func TestResponseTransformErrorReplacesSharedPipelineBodyThroughWrapper(t *testing.T) {
	p := newTestPlugin(t, Config{Response: &Transform{
		InputFormat: "json",
		Template:    `{"result":"{{message}}"}`,
	}})
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`not-json`))
	})
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := base.GetOrCreateTransformResponseWriter(r)
		wrapped := httpsnoop.Wrap(recorder, httpsnoop.Hooks{})
		p.Handler(downstream).ServeHTTP(wrapped, r)
		recorder.Commit(w)
	})
	handler := base.WithTransformPipeline(2)(outer)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "not-json") {
		t.Fatalf("response body leaked upstream body: %q", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidJSONRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name"`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body decode") {
		t.Fatalf("body = %q, want decode error", rr.Body.String())
	}
}

func TestClientControlBoundsBodyTransformerRead(t *testing.T) {
	client := &client_control.Plugin{}
	if err := client.Init(); err != nil {
		t.Fatalf("client-control Init() error = %v", err)
	}
	client.Config().(*client_control.Config).MaxBodySize = 4
	if err := client.PostInit(); err != nil {
		t.Fatalf("client-control PostInit() error = %v", err)
	}

	transformer := newTestPlugin(t, Config{
		Request: &Transform{InputFormat: "plain", Template: `{"body":"{{_body}}"}`},
	})
	nextCalled := false
	handler := client.Handler(transformer.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	})))
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader("too-large"))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("response code = %d, want 413", rr.Code)
	}
	if nextCalled {
		t.Fatal("transformer/downstream handler was called after client-control rejected the body")
	}
}

func TestHandlerRejectsMalformedTemplate(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"name":"{{name"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for malformed template")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "template") {
		t.Fatalf("body = %q, want template validation error", rr.Body.String())
	}
}

func TestEvaluateTemplateCondition(t *testing.T) {
	ctx := templateContext{
		values: map[string]string{
			"count":       "5",
			"name":        "alice",
			"missing":     "null",
			"empty_value": "",
		},
		body:   "payload",
		req:    httptest.NewRequest(http.MethodPost, "http://example.test/", nil),
		format: "json",
	}

	tests := []struct {
		name string
		expr string
		want bool
	}{
		{name: "equal", expr: `name == "alice"`, want: true},
		{name: "greater", expr: `count > 3`, want: true},
		{name: "greater or equal", expr: `count >= 5`, want: true},
		{name: "less", expr: `count < 3`},
		{name: "less or equal", expr: `count <= 4`},
		{name: "match", expr: `name ~= "ali"`, want: true},
		{name: "and", expr: `count > 3 and name == "alice"`, want: true},
		{name: "and short-circuit", expr: `count < 3 and name == "alice"`},
		{name: "or", expr: `count < 3 or name == "alice"`, want: true},
		{name: "or all false", expr: `count < 3 or name == "bob"`},
		{name: "not", expr: `not count < 3`, want: true},
		{name: "nil operand", expr: `missing == nil`, want: true},
		{name: "true literal", expr: `true`, want: true},
		{name: "false literal", expr: `false`},
		{name: "string literal", expr: `name == 'alice'`, want: true},
		{name: "body operand", expr: `_body == "payload"`, want: true},
		{name: "missing operand", expr: `missing_value == "x"`},
		{name: "non-numeric comparison", expr: `name > 3`},
		{name: "unknown operand", expr: `unknown == "x"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := evaluateTemplateCondition(test.expr, ctx); got != test.want {
				t.Fatalf("evaluateTemplateCondition(%q) = %t, want %t", test.expr, got, test.want)
			}
		})
	}
}

func TestTemplateStringLiteral(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want string
		ok   bool
	}{
		{name: "double quoted", expr: `"hello"`, want: "hello", ok: true},
		{name: "single quoted", expr: `'hello'`, want: "hello", ok: true},
		{name: "escaped newline", expr: `"a\nb"`, want: "a\nb", ok: true},
		{name: "escaped quote", expr: `"a\"b"`, want: `a"b`, ok: true},
		{name: "escaped backslash", expr: `"a\\b"`, want: `a\b`, ok: true},
		{name: "bell escape", expr: `"a\ab"`, want: "a\ab", ok: true},
		{name: "backspace", expr: `"a\bb"`, want: "a\bb", ok: true},
		{name: "form feed", expr: `"a\fb"`, want: "a\fb", ok: true},
		{name: "carriage return", expr: `"a\rb"`, want: "a\rb", ok: true},
		{name: "tab", expr: `"a\tb"`, want: "a\tb", ok: true},
		{name: "vertical tab", expr: `"a\vb"`, want: "a\vb", ok: true},
		{name: "unknown escape", expr: `"a\zb"`},
		{name: "unterminated escape", expr: `"a\`},
		{name: "too short", expr: `"`},
		{name: "unquoted", expr: "hello"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := templateStringLiteral(test.expr)
			if ok != test.ok || got != test.want {
				t.Fatalf("templateStringLiteral(%q) = %q/%t, want %q/%t", test.expr, got, ok, test.want, test.ok)
			}
		})
	}
}
