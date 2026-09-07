package function_upstream

import (
	"errors"
	"net/http/httptest"
	"testing"
)

type failedRequestBody struct{}

func (failedRequestBody) Read([]byte) (int, error) { return 0, errors.New("request body failed") }
func (failedRequestBody) Close() error             { return nil }

func TestRequestBodyReadErrorReturnsBadRequest(t *testing.T) {
	p := newTestPlugin(t, Config{FunctionURI: "http://function.invalid"})
	req := httptest.NewRequest("POST", "/function", nil)
	req.Body = failedRequestBody{}
	response := httptest.NewRecorder()
	p.RunRequestPhase(response, req)
	if response.Code != 400 {
		t.Fatalf("status=%d body=%q, want client error 400", response.Code, response.Body.String())
	}
}
