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
