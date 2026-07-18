package oas_validator

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
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

func TestHandlerValidatesInlineOpenAPISpec(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec:                testSpec(),
		VerboseErrors:       true,
		RejectionStatusCode: http.StatusUnprocessableEntity,
	})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("response code = %d, want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to validate request") ||
		!strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want verbose validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerPassesAndRestoresValidRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: testSpec()})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(body) != `{"name":"doggie"}` {
			t.Fatalf("restored body = %q, want original", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerMatchesOpenAPIServerURLPrefix(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: `{
  "openapi": "3.0.2",
  "servers": [{"url": "/api/v3"}],
  "paths": {
    "/pets": {
      "get": {"responses": {"204": {"description": "no content"}}}
    }
  }
}`})

	req := httptest.NewRequest(http.MethodGet, "/api/v3/pets", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerPrefersLiteralPathOverPathParameter(t *testing.T) {
	spec := &compiledSpec{operations: []compiledOperation{
		{
			method:   http.MethodGet,
			template: "/api/v31/pet/{petId}",
			segments: splitPath("/api/v31/pet/{petId}"),
		},
		{
			method:   http.MethodGet,
			template: "/api/v31/pet/findByStatus",
			segments: splitPath("/api/v31/pet/findByStatus"),
		},
	}}

	operation, _ := spec.match(http.MethodGet, "/api/v31/pet/findByStatus")
	if operation == nil || operation.template != "/api/v31/pet/findByStatus" {
		t.Fatalf("matched operation = %#v, want literal path", operation)
	}
}

func TestMetadataSchemaRejectsNonpositiveSpecURLTTL(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: testSpec()})
	metadataSchema := p.GetMetadataSchema()
	if err := util.Validate(map[string]any{"spec_url_ttl": 1}, metadataSchema); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	if err := util.Validate(map[string]any{"spec_url_ttl": 0}, metadataSchema); err == nil {
		t.Fatal("zero spec_url_ttl accepted")
	}
}

func TestHandlerValidatesRequestBodyWithLocalSchemaRef(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec:          testSpecWithComponentsRef(),
		VerboseErrors: true,
	})

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerResolvesLocalParameterRef(t *testing.T) {
	spec := `{
  "openapi": "3.0.2",
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {"$ref": "#/components/parameters/Trace"}
        ]
      }
    }
  },
  "components": {
    "parameters": {
      "Trace": {
        "name": "X-Trace",
        "in": "header",
        "required": true,
        "schema": {"type": "string", "minLength": 1}
      }
    }
  }
}`
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusBadRequest},
		{name: "present", header: "trace-id", wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pets", nil)
			if test.header != "" {
				req.Header.Set("X-Trace", test.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerResolvesLocalRequestBodyRef(t *testing.T) {
	spec := `{
  "openapi": "3.0.2",
  "paths": {
    "/pets": {
      "post": {
        "requestBody": {"$ref": "#/components/requestBodies/Pet"}
      }
    }
  },
  "components": {
    "requestBodies": {
      "Pet": {
        "required": true,
        "content": {
          "application/json": {
            "schema": {
              "type": "object",
              "required": ["name"],
              "properties": {"name": {"type": "string"}}
            }
          }
        }
      }
    }
  }
}`
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "missing", wantStatus: http.StatusBadRequest},
		{name: "present", body: `{"name":"doggie"}`, wantStatus: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerValidatesURLFormBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Spec: formSpec(),
	})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader("name=doggie&age=3"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid form body", rr.Code)
	}
}

func TestHandlerValidatesMultipartBody(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "doggie"); err != nil {
		t.Fatalf("WriteField(name) error = %v", err)
	}
	if err := writer.WriteField("age", "3"); err != nil {
		t.Fatalf("WriteField(age) error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	p := newTestPlugin(t, Config{Spec: strings.Replace(formSpec(),
		"application/x-www-form-urlencoded", "multipart/form-data", 1)})
	req := httptest.NewRequest(http.MethodPost, "/pets", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid multipart body", rr.Code)
	}
}

func TestHandlerValidatesPlainTextBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: plainTextSpec()})
	req := httptest.NewRequest(http.MethodPost, "/messages", strings.NewReader("hello apisix"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid text body", rr.Code)
	}
}

func TestHandlerValidatesXMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: xmlBodySpec()})
	req := httptest.NewRequest(
		http.MethodPost,
		"/users",
		strings.NewReader(
			`<user id="u-1"><name>alice</name><age>3</age><tags><tag>admin</tag><tag>viewer</tag></tags></user>`,
		),
	)
	req.Header.Set("Content-Type", "application/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid XML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesYAMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: yamlBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("name: alice\nage: 3\n"))
	req.Header.Set("Content-Type", "application/yaml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid YAML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedYAMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: yamlBodySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader("name: [alice\nage: 3\n"))
	req.Header.Set("Content-Type", "text/yaml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid YAML body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for invalid YAML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedXMLBody(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: xmlBodySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`<user><name>alice</name>`))
	req.Header.Set("Content-Type", "application/xml")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid XML body")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for invalid XML body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesStructuredJSONMediaTypeSuffix(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: jsonSuffixBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"id":"evt-1"}`))
	req.Header.Set("Content-Type", "application/problem+json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for +json body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerMatchesStructuredJSONWildcardMediaType(t *testing.T) {
	spec := strings.Replace(jsonSuffixBodySpec(), `"application/json"`, `"application/*+json"`, 1)
	p := newTestPlugin(t, Config{Spec: spec})
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"id":"evt-1"}`))
	req.Header.Set("Content-Type", "application/problem+json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for wildcard +json body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerMatchesStructuredXMLAndYAMLWildcardMediaTypes(t *testing.T) {
	tests := []struct {
		name        string
		spec        string
		contentType string
		body        string
		path        string
	}{
		{
			name:        "xml",
			spec:        strings.Replace(xmlBodySpec(), `"application/xml"`, `"application/*+xml"`, 1),
			contentType: "application/problem+xml",
			body:        `<user id="u-1"><name>alice</name><age>3</age><tags><tag>admin</tag><tag>viewer</tag></tags></user>`,
			path:        "/users",
		},
		{
			name:        "yaml",
			spec:        strings.Replace(yamlBodySpec(), `"application/yaml"`, `"application/*+yaml"`, 1),
			contentType: "application/problem+yaml",
			body:        "name: alice\nage: 3\n",
			path:        "/users",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: test.spec})
			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204 for wildcard %s body: %s", rr.Code, test.name, rr.Body.String())
			}
		})
	}
}

func TestSelectMediaTypePrefersSpecificWildcard(t *testing.T) {
	content := map[string]mediaType{
		"*/*":           {Schema: map[string]any{"title": "generic"}},
		"application/*": {Schema: map[string]any{"title": "application"}},
	}

	selected, ok := selectMediaType(content, "application/json")
	if !ok {
		t.Fatal("selectMediaType() found no matching wildcard")
	}
	if got := selected.Schema["title"]; got != "application" {
		t.Fatalf("selected schema title = %v, want application-specific wildcard", got)
	}
}

func TestHandlerValidatesOctetStreamBodyAsString(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: octetStreamBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/blobs", strings.NewReader("binary-payload"))
	req.Header.Set("Content-Type", "application/octet-stream")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for octet-stream body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesCustomOpaqueMediaBodyAsString(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: customOpaqueBodySpec()})
	req := httptest.NewRequest(http.MethodPost, "/payload", strings.NewReader("csv-like,payload\n"))
	req.Header.Set("Content-Type", "text/csv")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for opaque custom media body: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesSpaceDelimitedQueryArray(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: styledQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=red%20blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid space-delimited query array", rr.Code)
	}
}

func TestHandlerValidatesDelimitedQueryObject(t *testing.T) {
	for _, test := range []struct {
		name  string
		style string
		query string
	}{
		{name: "space", style: "spaceDelimited", query: "filter=role+admin+age+3"},
		{name: "pipe", style: "pipeDelimited", query: "filter=role%7Cadmin%7Cage%7C3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: delimitedQueryObjectSpec(test.style)})
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf(
					"response code = %d, want 204 for %s-delimited query object: %s",
					rr.Code,
					test.style,
					rr.Body.String(),
				)
			}
		})
	}
}

func TestHandlerRejectsMalformedDelimitedQueryObject(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: delimitedQueryObjectSpec("space"), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=role+admin+age", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for malformed delimited query object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerFlattensRepeatedDelimitedQueryArrayValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		style string
		query string
	}{
		{name: "space", style: "spaceDelimited", query: "tags=red+blue&tags=green+yellow"},
		{name: "pipe", style: "pipeDelimited", query: "tags=red%7Cblue&tags=green%7Cyellow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: repeatedDelimitedQuerySpec(test.style)})
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf(
					"response code = %d, want 204 for repeated %s-delimited values: %s",
					rr.Code,
					test.style,
					rr.Body.String(),
				)
			}
		})
	}
}

func TestHandlerValidatesDeepObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter%5Bname%5D=doggie&filter%5Bage%5D=3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for valid deepObject query parameter: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedDeepObjectScalarProperty(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(
		http.MethodGet,
		"/pets?filter%5Bname%5D=doggie&filter%5Bname%5D=cat&filter%5Bage%5D=3",
		nil,
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for a repeated deepObject scalar property")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "filter.name") {
		t.Fatalf("response body = %q, want repeated-property error", rr.Body.String())
	}
}

func TestHandlerValidatesJSONContentQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: jsonContentQueryParameterSpec(), VerboseErrors: true})
	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{
			name:       "valid object",
			query:      `coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "missing required property",
			query:      `coordinates=%7B%22lat%22%3A1.5%7D`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed JSON",
			query:      `coordinates=not-json`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "repeated parameter",
			query:      `coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D&coordinates=%7B%22lat%22%3A1.5%2C%22long%22%3A2.5%7D`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pets?"+test.query, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerValidatesTextContentQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: textContentQueryParameterSpec()})
	for _, test := range []struct {
		name       string
		value      string
		wantStatus int
	}{
		{name: "valid integer", value: "3", wantStatus: http.StatusNoContent},
		{name: "invalid integer", value: "not-an-integer", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/pets?limit="+test.value, nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != test.wantStatus {
				t.Fatalf("response code = %d, want %d: %s", rr.Code, test.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestHandlerRejectsUnsupportedContentQueryMediaType(t *testing.T) {
	spec := strings.Replace(textContentQueryParameterSpec(), "text/plain", "application/xml", 1)
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?limit=3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for unsupported parameter content media type")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unsupported parameter content media type") {
		t.Fatalf("response body = %q, want explicit unsupported-media error", rr.Body.String())
	}
}

func TestHandlerValidatesExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: formObjectQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?role=admin&first=Alex", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for exploded form object", rr.Code)
	}
}

func TestHandlerValidatesFreeFormExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: freeFormExplodedFormObjectQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?role=admin&tenant=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for free-form exploded form object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesNonExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,age,3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for non-exploded form object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMalformedNonExplodedFormObjectQueryParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,age", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for malformed non-exploded form object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "filter") {
		t.Fatalf("response body = %q, want parameter name", rr.Body.String())
	}
}

func TestHandlerRejectsDuplicateNonExplodedFormObjectField(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: nonExplodedFormObjectQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter=name,Alex,name,Bob,age,3", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for a duplicate object field")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "duplicate field") {
		t.Fatalf("response body = %q, want duplicate-field error", rr.Body.String())
	}
}

func TestHandlerValidatesMatrixLabelAndSimplePathParameters(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: styledPathSpec()})
	req := httptest.NewRequest(
		http.MethodGet,
		"/pets/;id=3/.red.blue/role=admin,first=Alex",
		nil,
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for matrix/label/simple path parameters: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsParameterStylesUnsupportedForLocation(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		style       string
		requestPath string
		query       string
		header      string
	}{
		{
			name:        "query matrix",
			in:          "query",
			style:       "matrix",
			requestPath: "/pets",
			query:       "?id=3",
		},
		{
			name:        "path form",
			in:          "path",
			style:       "form",
			requestPath: "/pets/3",
		},
		{
			name:        "header form",
			in:          "header",
			style:       "form",
			requestPath: "/pets",
			header:      "3",
		},
		{
			name:        "cookie simple",
			in:          "cookie",
			style:       "simple",
			requestPath: "/pets",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Spec:          invalidParameterStyleSpec(test.in, test.style),
				VerboseErrors: true,
			})
			req := httptest.NewRequest(http.MethodGet, test.requestPath+test.query, nil)
			if test.header != "" {
				req.Header.Set("X-ID", test.header)
			}
			if test.in == "cookie" {
				req.Header.Set("Cookie", "id=3")
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler was called for an unsupported parameter style")
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "unsupported") {
				t.Fatalf("response body = %q, want unsupported-style error", rr.Body.String())
			}
		})
	}
}

func TestValidateParameterStyleRejectsSchemaMismatch(t *testing.T) {
	tests := []struct {
		name   string
		style  string
		schema map[string]any
	}{
		{name: "deep object primitive", style: "deepObject", schema: map[string]any{"type": "string"}},
		{name: "space delimited primitive", style: "spaceDelimited", schema: map[string]any{"type": "string"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateParameterStyle(parameter{Style: test.style, Schema: test.schema}, "query")
			if err == nil {
				t.Fatalf("validateParameterStyle() error = nil, want schema mismatch rejection")
			}
			if !strings.Contains(err.Error(), "schema type") {
				t.Fatalf("validateParameterStyle() error = %q, want schema type context", err)
			}
		})
	}
}

func TestValidateParameterStyleRejectsNonExplodedDeepObject(t *testing.T) {
	err := validateParameterStyle(parameter{
		Style:   "deepObject",
		Explode: new(false),
		Schema:  map[string]any{"type": "object"},
	}, "query")
	if err == nil {
		t.Fatal("validateParameterStyle() error = nil, want deepObject explode=false rejection")
	}
	if !strings.Contains(err.Error(), "explode") {
		t.Fatalf("validateParameterStyle() error = %q, want explode context", err)
	}
}

func TestHandlerValidatesExplodedSimpleHeaderObject(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: simpleHeaderObjectSpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets", nil)
	req.Header.Set("X-Filter", "role=admin,first=Alex")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for simple header object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerValidatesCookieParameterStyles(t *testing.T) {
	tests := []struct {
		name   string
		spec   string
		cookie string
	}{
		{
			name:   "exploded object",
			spec:   explodedCookieObjectSpec(),
			cookie: "role=admin; first=Alex",
		},
		{
			name:   "non-exploded object",
			spec:   nonExplodedCookieObjectSpec(),
			cookie: "filter=name,Alex,age,3",
		},
		{
			name:   "repeated array",
			spec:   repeatedCookieArraySpec(),
			cookie: "tags=red; tags=blue",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Spec: test.spec})
			req := httptest.NewRequest(http.MethodGet, "/pets", nil)
			req.Header.Set("Cookie", test.cookie)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf("response code = %d, want 204 for %s cookie: %s", rr.Code, test.name, rr.Body.String())
			}
		})
	}
}

func TestHandlerPreservesCommaInExplodedFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: explodedFormArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=a%2Cb", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf(
			"response code = %d, want 204 for one exploded array value containing a comma: %s",
			rr.Code,
			rr.Body.String(),
		)
	}
}

func TestHandlerUsesDefaultExplodeForFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: defaultExplodedFormArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=a%2Cb", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for default exploded form array: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsRepeatedNonExplodedFormArrayQuery(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: repeatedNonExplodedFormArrayQuerySpec(), VerboseErrors: true})
	req := httptest.NewRequest(http.MethodGet, "/pets?tags=red&tags=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for a repeated non-exploded form array")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "must appear once") {
		t.Fatalf("response body = %q, want repeated-parameter error", rr.Body.String())
	}
}

func TestHandlerValidatesRepeatedDeepObjectArrayValues(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: deepObjectArrayQuerySpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets?filter%5Btags%5D=red&filter%5Btags%5D=blue", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204 for repeated deepObject array values: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsInvalidCookieParameter(t *testing.T) {
	p := newTestPlugin(t, Config{Spec: explodedCookieObjectSpec()})
	req := httptest.NewRequest(http.MethodGet, "/pets", nil)
	req.Header.Set("Cookie", "role=admin")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for an invalid cookie object")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400 for an invalid cookie object: %s", rr.Code, rr.Body.String())
	}
}

func TestHandlerCanSkipValidationAndAllowMismatch(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "skip query header body",
			cfg: Config{
				Spec:                        testSpec(),
				SkipRequestBodyValidation:   true,
				SkipRequestHeaderValidation: true,
				SkipQueryParamValidation:    true,
			},
		},
		{
			name: "log only mismatch",
			cfg: Config{
				Spec:             testSpec(),
				RejectIfNotMatch: new(false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.cfg)
			req := httptest.NewRequest(http.MethodPost, "/pets/123", strings.NewReader(`{"age":3}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusAccepted {
				t.Fatalf("response code = %d, want 202", rr.Code)
			}
		})
	}
}

func TestHandlerFetchesSpecURLWithConfiguredHeaders(t *testing.T) {
	var sawAuth bool
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "Bearer spec-token" {
			sawAuth = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{
		SpecURL: specServer.URL,
		SpecURLRequestHeaders: map[string]string{
			"Authorization": "Bearer spec-token",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Trace", "trace-id")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201", rr.Code)
	}
	if !sawAuth {
		t.Fatal("spec_url request missing configured Authorization header")
	}
}

func TestHandlerRefreshesSpecURLAfterMetadataTTL(t *testing.T) {
	var fetches atomic.Int32
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{SpecURL: specServer.URL})
	p.metadata.SpecURLTTL = 10
	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	serve := func() {
		req := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", strings.NewReader(`{"name":"doggie"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Trace", "trace-id")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("response code = %d, want 204", recorder.Code)
		}
	}

	serve()
	serve()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches after cached request = %d, want 1", got)
	}

	now = now.Add(11 * time.Second)
	serve()
	if got := fetches.Load(); got != 2 {
		t.Fatalf("fetches after TTL expiry = %d, want 2", got)
	}
}

func TestHandlerResolvesExternalSchemaRef(t *testing.T) {
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/schemas/pet.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name": {"type": "string"}
        }
      }
    }
  }
}`))
	}))
	defer externalServer.Close()

	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		externalServer.URL+"/schemas/pet.json#/components/schemas/Pet",
		1,
	)
	p := newTestPlugin(t, Config{Spec: spec, VerboseErrors: true})

	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want external schema validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerResolvesRelativeExternalSchemaRefFromSpecURL(t *testing.T) {
	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		"schemas/pet.json#/components/schemas/Pet",
		1,
	)
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/openapi.json":
			_, _ = w.Write([]byte(spec))
		case "/schemas/pet.json":
			_, _ = w.Write([]byte(`{
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {"name": {"type": "string"}}
      }
    }
  }
}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer specServer.Close()

	p := newTestPlugin(t, Config{
		SpecURL:       specServer.URL + "/openapi.json",
		VerboseErrors: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler was called for invalid request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "name") {
		t.Fatalf("response body = %q, want relative external schema validation error mentioning name", rr.Body.String())
	}
}

func TestHandlerRejectsExternalSchemaRefCycle(t *testing.T) {
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/a.json":
			_, _ = w.Write([]byte(`{"$ref":"b.json"}`))
		case "/b.json":
			_, _ = w.Write([]byte(`{"$ref":"a.json"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer specServer.Close()

	spec := `{
  "openapi": "3.0.2",
  "paths": {
    "/pets": {
      "post": {
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {"$ref": "` + specServer.URL + `/a.json"}
            }
          }
        }
      }
    }
  }
}`
	p := newTestPlugin(t, Config{Spec: spec})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for cyclic external ref")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
		t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
	}
}

func TestHandlerRejectsMissingExternalSchemaRef(t *testing.T) {
	externalServer := httptest.NewServer(http.NotFoundHandler())
	defer externalServer.Close()

	spec := strings.Replace(
		testSpecWithComponentsRef(),
		"#/components/schemas/Pet",
		externalServer.URL+"/missing.json#/components/schemas/Pet",
		1,
	)
	p := newTestPlugin(t, Config{Spec: spec})
	req := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"name":"doggie"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for missing external ref")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to parse openapi spec") {
		t.Fatalf("response body = %q, want spec parse failure", rr.Body.String())
	}
}

func testSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets/{id}": {
      "post": {
        "parameters": [
          {"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "trace", "in": "header", "required": true, "schema": {"type": "string"}},
          {"name": "verbose", "in": "query", "required": true, "schema": {"type": "string"}}
        ],
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "required": ["name"],
                "properties": {
                  "name": {"type": "string"},
                  "age": {"type": "integer"}
                }
              }
            }
          }
        },
        "responses": {"200": {"description": "OK"}}
      }
    }
  }
}`
}

func testSpecWithComponentsRef() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {"$ref": "#/components/schemas/Pet"}
            }
          }
        },
        "responses": {"200": {"description": "OK"}}
      }
    }
  },
  "components": {
    "schemas": {
      "Pet": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name": {"type": "string"},
          "age": {"type": "integer"}
        }
      }
    }
  }
}`
}

func formSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/x-www-form-urlencoded": {
              "schema": {
                "type": "object",
                "required": ["name"],
                "properties": {
                  "name": {"type": "string"},
                  "age": {"type": "integer"}
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func styledQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "style": "spaceDelimited",
            "schema": {"type": "array", "items": {"type": "string"}}
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func repeatedDelimitedQuerySpec(style string) string {
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": %q,
            "schema": {
              "type": "array",
              "minItems": 4,
              "maxItems": 4,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, style)
}

func delimitedQueryObjectSpec(style string) string {
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": %q,
            "schema": {
              "type": "object",
              "required": ["role", "age"],
              "properties": {
                "role": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, style)
}

func deepObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "style": "deepObject",
            "schema": {
              "type": "object",
              "required": ["name", "age"],
              "properties": {
                "name": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func jsonContentQueryParameterSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "coordinates",
            "in": "query",
            "required": true,
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "required": ["lat", "long"],
                  "properties": {
                    "lat": {"type": "number"},
                    "long": {"type": "number"}
                  }
                }
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func textContentQueryParameterSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "limit",
            "in": "query",
            "required": true,
            "content": {
              "text/plain": {
                "schema": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func formObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func freeFormExplodedFormObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "minProperties": 2,
              "additionalProperties": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func nonExplodedFormObjectQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": false,
            "schema": {
              "type": "object",
              "required": ["name", "age"],
              "properties": {
                "name": {"type": "string"},
                "age": {"type": "integer"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func styledPathSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets/{id}/{tags}/{filter}": {
      "get": {
        "parameters": [
          {
            "name": "id",
            "in": "path",
            "required": true,
            "style": "matrix",
            "schema": {"type": "integer"}
          },
          {
            "name": "tags",
            "in": "path",
            "required": true,
            "style": "label",
            "explode": true,
            "schema": {"type": "array", "items": {"type": "string"}, "minItems": 2}
          },
          {
            "name": "filter",
            "in": "path",
            "required": true,
            "style": "simple",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func invalidParameterStyleSpec(location, style string) string {
	path := "/pets"
	if location == "path" {
		path = "/pets/{id}"
	}
	return fmt.Sprintf(`{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    %q: {
      "get": {
        "parameters": [
          {
            "name": %q,
            "in": %q,
            "required": true,
            "style": %q,
            "schema": {"type": "integer"}
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`, path, map[string]string{
		"query":  "id",
		"path":   "id",
		"header": "X-ID",
		"cookie": "id",
	}[location], location, style)
}

func simpleHeaderObjectSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "X-Filter",
            "in": "header",
            "required": true,
            "style": "simple",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func explodedCookieObjectSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "cookie",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "object",
              "required": ["role", "first"],
              "properties": {
                "role": {"type": "string"},
                "first": {"type": "string"}
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func nonExplodedCookieObjectSpec() string {
	return strings.Replace(nonExplodedFormObjectQuerySpec(), `"in": "query"`, `"in": "cookie"`, 1)
}

func repeatedCookieArraySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "cookie",
            "required": true,
            "schema": {
              "type": "array",
              "minItems": 2,
              "maxItems": 2,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func explodedFormArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": true,
            "schema": {
              "type": "array",
              "minItems": 1,
              "maxItems": 1,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func defaultExplodedFormArrayQuerySpec() string {
	return strings.Replace(explodedFormArrayQuerySpec(), "            \"explode\": true,\n", "", 1)
}

func repeatedNonExplodedFormArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "tags",
            "in": "query",
            "required": true,
            "style": "form",
            "explode": false,
            "schema": {
              "type": "array",
              "minItems": 2,
              "maxItems": 2,
              "items": {"type": "string"}
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func deepObjectArrayQuerySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Pet API", "version": "1.0.0"},
  "paths": {
    "/pets": {
      "get": {
        "parameters": [
          {
            "name": "filter",
            "in": "query",
            "required": true,
            "style": "deepObject",
            "schema": {
              "type": "object",
              "required": ["tags"],
              "properties": {
                "tags": {
                  "type": "array",
                  "minItems": 2,
                  "items": {"type": "string"}
                }
              }
            }
          }
        ],
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func jsonSuffixBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Event API", "version": "1.0.0"},
  "paths": {
    "/events": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "required": ["id"],
                "properties": {"id": {"type": "string"}}
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func octetStreamBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Blob API", "version": "1.0.0"},
  "paths": {
    "/blobs": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/octet-stream": {
              "schema": {"type": "string", "minLength": 1}
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func customOpaqueBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Opaque API", "version": "1.0.0"},
  "paths": {
    "/payload": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "text/csv": {
              "schema": {"type": "string", "minLength": 1}
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func plainTextSpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "Message API", "version": "1.0.0"},
  "paths": {
    "/messages": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "text/plain": {"schema": {"type": "string", "minLength": 1}}
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func yamlBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "User API", "version": "1.0.0"},
  "paths": {
    "/users": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/yaml": {
              "schema": {
                "type": "object",
                "required": ["name", "age"],
                "properties": {
                  "name": {"type": "string", "minLength": 1},
                  "age": {"type": "integer", "minimum": 1}
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}

func xmlBodySpec() string {
	return `{
  "openapi": "3.0.2",
  "info": {"title": "User API", "version": "1.0.0"},
  "paths": {
    "/users": {
      "post": {
        "requestBody": {
          "required": true,
          "content": {
            "application/xml": {
              "schema": {
                "type": "object",
                "required": ["id", "name", "age", "tags"],
                "properties": {
                  "id": {"type": "string", "minLength": 1, "xml": {"attribute": true}},
                  "name": {"type": "string", "minLength": 1},
                  "age": {"type": "integer", "minimum": 1},
                  "tags": {
                    "type": "array",
                    "minItems": 2,
                    "items": {"type": "string"},
                    "xml": {"wrapped": true}
                  }
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "No Content"}}
      }
    }
  }
}`
}
