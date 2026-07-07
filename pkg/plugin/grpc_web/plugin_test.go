package grpc_web

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRespondsToCORSPreflight(t *testing.T) {
	p := newTestPlugin(t, Config{CorsAllowHeaders: "content-type,x-grpc-web,authorization"})

	req := httptest.NewRequest(http.MethodOptions, "/hello.Service/Method", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Methods"); got != http.MethodPost {
		t.Fatalf("Access-Control-Allow-Methods = %q, want POST", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Headers"); got != "content-type,x-grpc-web,authorization" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want configured headers", got)
	}
	if got := res.Header().Get("Access-Control-Expose-Headers"); got != "grpc-message,grpc-status" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want grpc-message,grpc-status", got)
	}
}

func TestHandlerRejectsInvalidRequest(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			name:        "non-post",
			method:      http.MethodGet,
			contentType: "application/grpc-web",
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "unsupported content type",
			method:      http.MethodPost,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid base64 text body",
			method:      http.MethodPost,
			contentType: "application/grpc-web-text",
			body:        "not-base64!",
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{})
			req := httptest.NewRequest(tt.method, "/hello.Service/Method", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			res := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(res, req)

			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tt.wantStatus)
			}
		})
	}
}

func TestHandlerTransformsTextRequestAndResponse(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(
		http.MethodPost,
		"/hello.Service/Method",
		strings.NewReader(base64.StdEncoding.EncodeToString([]byte("hello"))),
	)
	req.Header.Set("Content-Type", "application/grpc-web-text")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("upstream body = %q, want decoded hello", body)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}
		if got := r.Header.Get("TE"); got != "trailers" {
			t.Fatalf("upstream TE = %q, want trailers", got)
		}

		w.Header().Set("Content-Type", "application/grpc")
		_, _ = w.Write([]byte("reply"))
		w.Header().Set("Grpc-Status", "0")
		w.Header().Set("Grpc-Message", "ok")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/grpc-web-text" {
		t.Fatalf("response Content-Type = %q, want application/grpc-web-text", got)
	}
	wantBody := base64.StdEncoding.EncodeToString([]byte("reply")) +
		base64.StdEncoding.EncodeToString(buildTrailerForTest("0", "ok"))
	if got := res.Body.String(); got != wantBody {
		t.Fatalf("response body = %q, want %q", got, wantBody)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed", got)
	}
}

func TestHandlerTransformsBinaryRequestAndResponse(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/hello.Service/Method", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("upstream body = %q, want unchanged hello", body)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}

		w.Header().Set("Content-Type", "application/grpc")
		_, _ = w.Write([]byte("reply"))
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Message", "denied")
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Type"); got != "application/grpc-web+proto" {
		t.Fatalf("response Content-Type = %q, want application/grpc-web+proto", got)
	}
	wantBody := append([]byte("reply"), buildTrailerForTest("7", "denied")...)
	if got := res.Body.Bytes(); string(got) != string(wantBody) {
		t.Fatalf("response body = %q, want %q", got, wantBody)
	}
}

func buildTrailerForTest(status string, message string) []byte {
	trailer := []byte("grpc-status:" + status + "\r\n" + "grpc-message:" + message + "\r\n")
	return append([]byte{0x80, 0x00, 0x00, 0x00, byte(len(trailer))}, trailer...)
}
