package client_control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientControlHandlerBodyLimits(t *testing.T) {
	tests := []struct {
		name       string
		limit      int64
		body       string
		wantStatus int
		wantBody   string
		wantNext   bool
	}{
		{name: "disabled", body: "payload", wantStatus: http.StatusNoContent, wantBody: "payload", wantNext: true},
		{
			name:       "within limit",
			limit:      7,
			body:       "payload",
			wantStatus: http.StatusNoContent,
			wantBody:   "payload",
			wantNext:   true,
		},
		{name: "over limit", limit: 3, body: "payload", wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			plugin := &Plugin{}
			plugin.config = Config{MaxBodySize: test.limit}
			handler := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("downstream read body: %v", err)
				}
				if string(body) != test.wantBody {
					t.Fatalf("downstream body = %q, want %q", body, test.wantBody)
				}
				w.WriteHeader(http.StatusNoContent)
			}))

			request := httptest.NewRequest(http.MethodPost, "http://example.test/upload", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || called != test.wantNext {
				t.Fatalf("status/called = %d/%t, want %d/%t", response.Code, called, test.wantStatus, test.wantNext)
			}
		})
	}
}

func TestClientControlMaxBytesAtBoundary(t *testing.T) {
	called := false
	plugin := &Plugin{}
	plugin.config = Config{MaxBodySize: 7}
	handler := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "http://example.test/upload", strings.NewReader("payload"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("status/called = %d/%t, want 204/true at the exact limit", response.Code, called)
	}
}

func TestClientControlMaxBytesOneByteOver(t *testing.T) {
	called := false
	plugin := &Plugin{}
	plugin.config = Config{MaxBodySize: 6}
	handler := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "http://example.test/upload", strings.NewReader("payload"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("status/called = %d/%t, want 413/false one byte over the limit", response.Code, called)
	}
}

func TestClientControlMaxBytesReadFailureReturns500(t *testing.T) {
	called := false
	plugin := &Plugin{}
	plugin.config = Config{MaxBodySize: 1024}
	handler := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "http://example.test/upload", io.NopCloser(failingReader{}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || called {
		t.Fatalf("status/called = %d/%t, want 500/false for a body read failure", response.Code, called)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestIsChunkedRequest(t *testing.T) {
	tests := []struct {
		name        string
		transfer    []string
		headerValue string
		want        bool
	}{
		{name: "transfer encoding", transfer: []string{"chunked"}, want: true},
		{name: "mixed-case header", headerValue: "Chunked", want: true},
		{name: "neither", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://example.test/upload", nil)
			request.TransferEncoding = test.transfer
			request.Header.Set("Transfer-Encoding", test.headerValue)
			if got := isChunkedRequest(request); got != test.want {
				t.Fatalf("isChunkedRequest() = %t, want %t", got, test.want)
			}
		})
	}
}
