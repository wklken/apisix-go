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

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/client_control"
)

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
			Template:    `{"full_name":"{{name}}","raw":{{_escape_json(_body)}}}`,
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
