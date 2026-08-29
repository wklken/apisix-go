package grpc_web

import (
	"encoding/base64"
	"net/http/httptest"
	"testing"
)

// APISIX 3.17 at 9ef2ecab67f652d38365049613610ef649bb4ad0
// Base64-encodes every body-filter invocation and the final trailer separately.
func TestAPISIX317TextResponseBase64EncodesEachWriteAndTrailerIndependently(t *testing.T) {
	response := httptest.NewRecorder()
	writer := newStreamingResponseWriter(
		response,
		"application/grpc-web-text+proto",
		encodingBase64,
		nil,
	)
	writer.Header().Set("Grpc-Status", "0")

	for _, chunk := range [][]byte{[]byte("a"), []byte("bc")} {
		if written, err := writer.Write(chunk); err != nil || written != len(chunk) {
			t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", chunk, written, err, len(chunk))
		}
	}
	if err := writer.finish(); err != nil {
		t.Fatalf("finish() error = %v", err)
	}

	want := base64.StdEncoding.EncodeToString([]byte("a")) +
		base64.StdEncoding.EncodeToString([]byte("bc")) +
		base64.StdEncoding.EncodeToString(buildTrailerForTest("0", ""))
	if got := response.Body.String(); got != want {
		t.Fatalf("text response = %q, want independently encoded writes and trailer %q", got, want)
	}

	joinedBody := base64.StdEncoding.EncodeToString([]byte("abc")) +
		base64.StdEncoding.EncodeToString(buildTrailerForTest("0", ""))
	if response.Body.String() == joinedBody {
		t.Fatal("text response encoded the joined upstream body instead of each upstream write")
	}
}
