package log

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type panicWireBody struct{}

func (panicWireBody) Read([]byte) (int, error) { panic("wire size estimator read request body") }
func (panicWireBody) Close() error             { return nil }

func TestEstimateHTTP1RequestLengthMatchesControlledWireAndNeverReadsBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example.test/opentracing", nil)
	request.RequestURI = "/opentracing"
	request.Header.Set("User-Agent", "Go-http-client/1.1")

	if got, known := EstimateHTTP1RequestLength(request); !known || got != 89 {
		t.Fatalf("request length = %d/%t, want 89/true", got, known)
	}
	request.Close = true
	if got, known := EstimateHTTP1RequestLength(request); !known || got != 108 {
		t.Fatalf("closed request length = %d/%t, want 108/true", got, known)
	}
	request.Close = false
	request.Body = panicWireBody{}
	request.ContentLength = 7
	if got, known := EstimateHTTP1RequestLength(request); !known || got != 115 {
		t.Fatalf("request with body length = %d/%t, want 115/true", got, known)
	}
}

func TestEstimateHTTP1ResponseLengthMatchesNetHTTPBufferedFraming(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example.test/opentracing", nil)
	header := http.Header{
		"Content-Type": {"text/plain"},
		"Server":       {"APISIX-test"},
	}
	outcome := apisixctx.ResponseOutcome{Status: http.StatusOK, Bytes: 11}
	if got, known := EstimateHTTP1ResponseLength(
		request,
		header,
		outcome,
		[]byte("opentracing"),
	); !known ||
		got != 134 {
		t.Fatalf("response length = %d/%t, want 134/true", got, known)
	}

	autoHeaderOutcome := apisixctx.ResponseOutcome{Status: http.StatusCreated, Bytes: 5}
	if got, known := EstimateHTTP1ResponseLength(
		request,
		nil,
		autoHeaderOutcome,
		[]byte("reply"),
	); !known ||
		got != 126 {
		t.Fatalf("auto-header response length = %d/%t, want 126/true", got, known)
	}
}

func TestEstimateHTTP1WireLengthsFailClosedForUnrecoverableFraming(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://gateway.example.test/", nil)
	request.ProtoMajor = 2
	request.ProtoMinor = 0
	if _, known := EstimateHTTP1RequestLength(request); known {
		t.Fatal("HTTP/2 request length unexpectedly reported as exact HTTP/1.1 wire size")
	}
	request.ProtoMajor = 1
	request.ProtoMinor = 1
	for _, outcome := range []apisixctx.ResponseOutcome{
		{Status: http.StatusOK, Bytes: 1, Flushed: true},
		{Status: http.StatusOK, Bytes: 1, Hijacked: true},
		{Status: http.StatusOK, Bytes: http1ServerBufferBytes + 1},
	} {
		if _, known := EstimateHTTP1ResponseLength(request, nil, outcome, []byte("x")); known {
			t.Fatalf("unrecoverable outcome %+v unexpectedly reported exact wire size", outcome)
		}
	}
}
