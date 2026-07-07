package http_logger

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
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

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})

	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want official default 3 seconds", p.config.Timeout)
	}
	if p.config.ConcatMethod != "json" {
		t.Fatalf("concat_method = %q, want json", p.config.ConcatMethod)
	}
}

func TestSendPostsJSONLogWithAuthorizationHeader(t *testing.T) {
	authHeader := "Bearer secret"
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != authHeader {
			t.Fatalf("authorization = %q, want %q", r.Header.Get("Authorization"), authHeader)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:        server.URL + "/logs?source=apisix",
		AuthHeader: &authHeader,
		Timeout:    3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if body["path"] != "/orders" {
			t.Fatalf("body = %#v, want path /orders", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestPostInitSetsTextContentTypeForNewLineConcat(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		ConcatMethod: "new_line",
		Timeout:      3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case got := <-received:
		if got != "text/plain" {
			t.Fatalf("content-type = %q, want text/plain", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:              server.URL,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Body.String() != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream body preserved", rr.Body.String())
	}
	select {
	case body := <-upstreamBody:
		if body != `{"order":1}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}
