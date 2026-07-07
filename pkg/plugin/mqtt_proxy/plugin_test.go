package mqtt_proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestHandlerPassesThrough(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestPostInitFillsDefaultProtocolName(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.ProtocolName != "MQTT" {
		t.Fatalf("ProtocolName = %q, want MQTT", p.config.ProtocolName)
	}
}

func TestSchemaValidatesOfficialConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"protocol_name":  "MQTT",
		"protocol_level": 4,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("mqtt-proxy config should validate: %v", err)
	}
	if err := util.Validate(map[string]any{"protocol_name": "MQTT"}, p.GetSchema()); err == nil {
		t.Fatal("mqtt-proxy config without protocol_level should not validate")
	}
}
