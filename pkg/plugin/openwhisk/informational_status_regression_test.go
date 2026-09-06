package openwhisk

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContinueActionStatusClosesWithoutFinalResponse(t *testing.T) {
	p := &Plugin{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.writeActionResponse(
			w,
			&http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(`{"statusCode":100,"body":"pending"}`)),
			},
			false,
		)
	}))
	defer gateway.Close()
	response, err := gateway.Client().Get(gateway.URL)
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("response=%v err=%v; want no final response (EOF)", response, err)
	}
}
