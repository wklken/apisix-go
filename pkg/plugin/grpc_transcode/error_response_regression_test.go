package grpc_transcode

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestErrorResponsePreservesBodyAndHeaderMetadataWhenStatusBodyDisabled(t *testing.T) {
	restore := stubProtoContent(t, "error-preserve-proto", testDescriptorContent(t))
	defer restore()
	p := newTestPlugin(t, Config{ProtoID: "error-preserve-proto", Service: "echo.EchoService", Method: "Echo"})
	request := p.RunRequestPhase(httptest.NewRecorder(), httptest.NewRequest("GET", "/echo?msg=hello", nil)).Request
	for _, body := range []string{"", "upstream error bytes"} {
		state := base.ResponseState{
			Status: 200,
			Header: http.Header{
				"Content-Type":            {"application/grpc"},
				"Grpc-Status":             {"14"},
				"Grpc-Message":            {"try%20later"},
				"Grpc-Status-Details-Bin": {"opaque"},
			},
			Body: []byte(body),
		}
		if err := p.RunBufferedBodyFilter(request, &state); err != nil {
			t.Fatal(err)
		}
		if state.Status != 503 || string(state.Body) != body {
			t.Errorf("response=%d %q, want 503 %q", state.Status, state.Body, body)
		}
		for key, want := range map[string]string{"Grpc-Status": "14", "Grpc-Message": "try%20later", "Grpc-Status-Details-Bin": "opaque"} {
			if got := state.Header.Get(key); got != want {
				t.Errorf("header %s=%q, want %q", key, got, want)
			}
			if got := state.Trailer.Get(key); got != "" {
				t.Errorf("header moved to trailer %s=%q", key, got)
			}
		}
	}
}
