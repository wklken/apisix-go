package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
)

func TestControlHandlerServesExamplePluginHello(t *testing.T) {
	cfg := &config.Config{
		Apisix:  config.Apisix{EnableControl: true},
		Plugins: []string{"example-plugin"},
	}
	handler := newControlHandler(cfg, nil)

	text := httptest.NewRecorder()
	handler.ServeHTTP(text, httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello", nil))
	if text.Code != http.StatusOK {
		t.Fatalf("hello status = %d, want 200; body=%q", text.Code, text.Body.String())
	}
	if text.Body.String() != "world\n" {
		t.Fatalf("hello body = %q, want world newline", text.Body.String())
	}

	encoded := httptest.NewRecorder()
	handler.ServeHTTP(encoded, httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello?json", nil))
	if encoded.Code != http.StatusOK {
		t.Fatalf("json hello status = %d, want 200; body=%q", encoded.Code, encoded.Body.String())
	}
	if got := encoded.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("json hello Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.Unmarshal(encoded.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode json hello: %v; body=%q", err, encoded.Body.String())
	}
	if body["msg"] != "world" {
		t.Fatalf("json hello msg = %q, want world", body["msg"])
	}
}

func TestControlHandlerServesExampleHelloWithoutServerInfo(t *testing.T) {
	cfg := &config.Config{
		Apisix:  config.Apisix{EnableControl: true},
		Plugins: []string{"example-plugin"},
	}
	response := httptest.NewRecorder()
	newControlHandler(cfg, nil).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/server_info", nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("server-info status = %d, want 404 when plugin is disabled", response.Code)
	}
}

func TestControlHandlerKeepsServerInfoWhenExamplePluginEnabled(t *testing.T) {
	view := server_info.NewView("control-node")
	view.SetEtcdVersion("3.6.13")
	cfg := &config.Config{
		Apisix:  config.Apisix{EnableControl: true},
		Plugins: []string{"server-info", "example-plugin"},
	}
	handler := newControlHandler(cfg, view)

	info := httptest.NewRecorder()
	handler.ServeHTTP(info, httptest.NewRequest(http.MethodGet, "/v1/server_info", nil))
	if info.Code != http.StatusOK {
		t.Fatalf("server-info status = %d, want 200; body=%s", info.Code, info.Body.String())
	}
	var body server_info.Response
	if err := json.Unmarshal(info.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode server-info: %v", err)
	}
	if body.ID != "control-node" || body.EtcdVersion != "3.6.13" {
		t.Fatalf("server-info response = %+v, want shared view", body)
	}

	hello := httptest.NewRecorder()
	handler.ServeHTTP(hello, httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello", nil))
	if hello.Code != http.StatusOK || hello.Body.String() != "world\n" {
		t.Fatalf("hello = %d %q, want 200 world newline", hello.Code, hello.Body.String())
	}
}

func TestControlHandlerRejectsExampleHelloWhenPluginDisabled(t *testing.T) {
	cfg := &config.Config{
		Apisix:  config.Apisix{EnableControl: true},
		Plugins: []string{"server-info"},
	}
	view := server_info.NewView("control-node")
	response := httptest.NewRecorder()
	newControlHandler(cfg, view).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello", nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("hello status = %d, want 404 when example-plugin is disabled", response.Code)
	}
}

func TestControlHandlerIgnoresExampleHelloWhenControlDisabled(t *testing.T) {
	cfg := &config.Config{Plugins: []string{"example-plugin"}}
	response := httptest.NewRecorder()
	newControlHandler(cfg, nil).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/plugin/example-plugin/hello", nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("hello status = %d, want 404 when control is disabled", response.Code)
	}
}

func TestControlHandlerExampleHelloRejectsNonGET(t *testing.T) {
	cfg := &config.Config{
		Apisix:  config.Apisix{EnableControl: true},
		Plugins: []string{"example-plugin"},
	}
	response := httptest.NewRecorder()
	newControlHandler(cfg, nil).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1/plugin/example-plugin/hello", strings.NewReader("{}")),
	)
	if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST hello status = %d, want 404 or 405", response.Code)
	}
}
