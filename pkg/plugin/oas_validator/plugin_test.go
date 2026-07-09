package oas_validator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
				RejectIfNotMatch: boolPtr(false),
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

func boolPtr(v bool) *bool {
	return &v
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
