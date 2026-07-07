package gm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestPlugin(t *testing.T) *Plugin {
	t.Helper()

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerPassesThrough(t *testing.T) {
	p := newTestPlugin(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GM-Test", "next")
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	if rr.Header().Get("X-GM-Test") != "next" {
		t.Fatalf("pass-through header = %q, want next", rr.Header().Get("X-GM-Test"))
	}
}

func TestValidateSSLConfigAllowsOrdinarySSL(t *testing.T) {
	cfg := SSLConfig{
		Cert: "enc cert",
		Key:  "enc key",
		SNIs: []string{"example.com"},
	}

	if err := ValidateSSLConfig(cfg); err != nil {
		t.Fatalf("ValidateSSLConfig() error = %v", err)
	}
}

func TestValidateSSLConfigRequiresSignPairForGM(t *testing.T) {
	cfg := SSLConfig{
		GM:   true,
		Cert: "enc cert",
		Key:  "enc key",
		SNIs: []string{"example.com"},
	}

	if err := ValidateSSLConfig(cfg); err == nil {
		t.Fatal("ValidateSSLConfig() error = nil, want sign cert/key requirement")
	}

	cfg.Certs = []string{"sign cert"}
	cfg.Keys = []string{"sign key"}
	if err := ValidateSSLConfig(cfg); err != nil {
		t.Fatalf("ValidateSSLConfig() with sign pair error = %v", err)
	}
}

func TestValidateSSLConfigRejectsWrongGMSignPairCount(t *testing.T) {
	cfg := SSLConfig{
		GM:    true,
		Cert:  "enc cert",
		Key:   "enc key",
		Certs: []string{"sign cert", "extra sign cert"},
		Keys:  []string{"sign key"},
	}

	if err := ValidateSSLConfig(cfg); err == nil {
		t.Fatal("ValidateSSLConfig() error = nil, want exact one sign cert/key requirement")
	}
}
