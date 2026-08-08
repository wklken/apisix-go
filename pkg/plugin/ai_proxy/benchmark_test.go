package ai_proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkProviderDispatch measures the per-request provider dispatch path:
// body decoding, provider request preparation, and endpoint resolution.
func BenchmarkProviderDispatch(b *testing.B) {
	p := &Plugin{config: Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Options:  map[string]any{"model": "gpt-4"},
		Override: Override{Endpoint: "https://provider.example/v1", LLMOptions: LLMOptions{MaxTokens: 512}},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"caller-model",
	  "messages":[{"role":"user","content":"hello"}],
	  "max_tokens": 64
	}`))
	req.Header.Set("Content-Type", "application/json")

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		body, document, protocol, err := p.readJSONDocument(req)
		if err != nil {
			b.Fatalf("readJSONDocument() error = %v", err)
		}
		prepared, err := p.prepareProviderRequest(body, document, protocol)
		if err != nil {
			b.Fatalf("prepareProviderRequest() error = %v", err)
		}
		if _, err := p.buildProviderRequestDocument(req, prepared.providerBody, prepared.providerDocument, prepared.providerProtocol); err != nil {
			b.Fatalf("buildProviderRequestDocument() error = %v", err)
		}
	}
}

// BenchmarkProviderDispatchErrorClass measures status classification of
// request errors; it must not depend on error text.
func BenchmarkProviderDispatchErrorClass(b *testing.B) {
	p := &Plugin{config: Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
		Override: Override{Endpoint: "https://provider.example/v1"},
	}}
	if err := p.Init(); err != nil {
		b.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		b.Fatalf("PostInit() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 17
	p.config.MaxReqBodySize = 4

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _, err := p.readJSONDocument(req)
		if err == nil {
			b.Fatal("readJSONDocument() accepted an oversized body")
		}
	}
}
