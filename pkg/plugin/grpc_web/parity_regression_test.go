package grpc_web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParityNoDataLateTrailers(t *testing.T) {
	r := httptest.NewRecorder()
	w := newStreamingResponseWriter(r, "application/grpc-web", encodingBinary, nil)
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
	w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if got := r.Result().Header.Get("Grpc-Status"); got != "" {
		t.Fatalf("test requires initial headers without status: %s", got)
	}
	if !bytes.Equal(r.Body.Bytes(), buildTrailer("0", "")) {
		t.Fatalf("body=%x, want terminal gRPC-Web trailer %x", r.Body.Bytes(), buildTrailer("0", ""))
	}
}
