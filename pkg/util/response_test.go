package util

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONPreservesRouteErrorResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := WriteJSON(recorder, http.StatusBadGateway, `bad <upstream>`); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Body.String(); got != `"bad \u003cupstream\u003e"` {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteJSONDoesNotCommitHeaderWhenMarshalFails(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := WriteJSON(recorder, http.StatusBadGateway, func() {})
	if err == nil {
		t.Fatal("WriteJSON() error = nil")
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("response committed on marshal error: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestWriteJSONMessageWritesCanonicalMessageObject(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := WriteJSONMessage(recorder, http.StatusBadRequest, "bad <input> & retry"); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Body.String(); got != `{"message":"bad \u003cinput\u003e \u0026 retry"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestBuildMessageResponse(t *testing.T) {
	if got := BuildMessageResponse("not found"); got != "{\"message\":\"not found\"}" {
		t.Fatalf("BuildMessageResponse() = %q", got)
	}
}

func TestParseRoundTripsAndRejectsBadInput(t *testing.T) {
	type target struct {
		Count int `json:"count"`
	}
	var parsed target
	if err := Parse(map[string]any{"count": 2}, &parsed); err != nil {
		t.Fatalf("Parse(valid) error = %v", err)
	}
	if parsed.Count != 2 {
		t.Fatalf("parsed count = %d, want 2", parsed.Count)
	}

	if err := Parse(func() {}, &target{}); err == nil {
		t.Fatal("Parse(unsupported source) error = nil")
	}
	if err := Parse(map[string]any{"count": "two"}, &target{}); err == nil {
		t.Fatal("Parse(type mismatch) error = nil")
	}
}

func TestValidateAcceptsAndRejectsConfig(t *testing.T) {
	schema := "{\"type\":\"object\",\"properties\":{\"count\":{\"type\":\"integer\"}},\"required\":[\"count\"]}"
	if err := Validate(map[string]any{"count": 2}, schema); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}
	if err := Validate(map[string]any{"count": "two"}, schema); err == nil {
		t.Fatal("Validate(invalid) error = nil")
	}
	if err := Validate(nil, "{"); err == nil {
		t.Fatal("Validate(broken schema) error = nil")
	}
}

func TestCompiledSchemaAcceptsAndRejectsConfig(t *testing.T) {
	schema := "{\"type\":\"object\",\"properties\":{\"count\":{\"type\":\"integer\"}},\"required\":[\"count\"]}"
	compiled, err := CompileSchema(schema)
	if err != nil {
		t.Fatalf("CompileSchema error = %v", err)
	}
	if err := compiled.Validate(map[string]any{"count": 2}); err != nil {
		t.Fatalf("CompiledSchema.Validate(valid) error = %v", err)
	}
	if err := compiled.Validate(map[string]any{"count": "two"}); err == nil {
		t.Fatal("CompiledSchema.Validate(invalid) error = nil")
	}
	if err := compiled.Validate(nil); err == nil {
		t.Fatal("CompiledSchema.Validate(nil config) error = nil")
	}
	if _, err := CompileSchema("{"); err == nil {
		t.Fatal("CompileSchema(broken schema) error = nil")
	}
}

func TestCompiledSchemaErrorsMatchValidate(t *testing.T) {
	schema := "{\"type\":\"object\",\"properties\":{\"count\":{\"type\":\"integer\"}},\"required\":[\"count\"]}"
	compiled, err := CompileSchema(schema)
	if err != nil {
		t.Fatalf("CompileSchema error = %v", err)
	}
	compileErr := Validate(map[string]any{"count": "two"}, schema)
	compiledErr := compiled.Validate(map[string]any{"count": "two"})
	if compileErr == nil || compiledErr == nil {
		t.Fatal("validation error = nil")
	}
	if compileErr.Error() != compiledErr.Error() {
		t.Fatalf("error mismatch: Validate = %q, CompiledSchema = %q", compileErr.Error(), compiledErr.Error())
	}
	if _, err := CompileSchema("{"); err == nil {
		t.Fatal("CompileSchema(broken schema) error = nil")
	}
	if err := Validate(map[string]any{}, "{"); err == nil {
		t.Fatal("Validate(broken schema) error = nil")
	}
}
