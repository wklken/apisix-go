package request_validation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodySchemaRequiresBodyBeforeSchemaEvaluation(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema:   map[string]any{},
		RejectedCode: http.StatusUnprocessableEntity,
		RejectedMsg:  "body required",
	})
	for _, test := range []struct {
		name string
		body io.Reader
	}{
		{name: "nil", body: nil},
		{name: "zero bytes", body: strings.NewReader("")},
		{name: "whitespace", body: strings.NewReader(" \t\r\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", test.body)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("missing body reached downstream")
			})).ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity ||
				strings.TrimSpace(response.Body.String()) != "body required" {
				t.Fatalf("response = %d/%q, want 422/body required", response.Code, response.Body.String())
			}
		})
	}
}

func TestRequestValidationBodyMatrixKeepsJSONNullDistinctFromMissingBody(t *testing.T) {
	for _, test := range []struct {
		name       string
		schema     map[string]any
		body       string
		wantStatus int
	}{
		{name: "null allowed", schema: map[string]any{"type": []any{"null", "object"}}, body: "null", wantStatus: http.StatusNoContent},
		{name: "null rejected by object", schema: map[string]any{"type": "object"}, body: "null", wantStatus: http.StatusBadRequest},
		{name: "object accepted", schema: map[string]any{"type": "object"}, body: "{}", wantStatus: http.StatusNoContent},
		{name: "valid form", schema: map[string]any{"type": "object", "required": []any{"name"}}, body: "name=alice", wantStatus: http.StatusNoContent},
		{name: "invalid form", schema: map[string]any{"type": "object", "required": []any{"name"}}, body: "other=value", wantStatus: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{BodySchema: test.schema})
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			if strings.Contains(test.name, "form") {
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read downstream body: %v", err)
				}
				if strings.Contains(test.name, "accepted") || test.name == "null allowed" || test.name == "valid form" {
					if string(body) != test.body {
						t.Fatalf("downstream body = %q, want %q", body, test.body)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}
