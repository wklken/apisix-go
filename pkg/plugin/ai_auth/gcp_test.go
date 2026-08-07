package ai_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestGCPTokenSourceExchangesAndCachesServiceAccountToken(t *testing.T) {
	var tokenCalls atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if r.Form.Get("grant_type") != gcpJWTBearerGrantType || strings.Count(r.Form.Get("assertion"), ".") != 2 {
			t.Fatalf("token form = %#v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"gcp-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	serviceAccount, err := json.Marshal(map[string]any{
		"type":           "service_account",
		"client_email":   "service@example.test",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"private_key_id": "key-id",
		"token_uri":      tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	source := NewGCPTokenSource()
	source.now = func() time.Time { return time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC) }
	config := GCPConfig{ServiceAccountJSON: string(serviceAccount)}

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "https://vertex.example.test", nil)
		if err := source.Apply(req.Context(), tokenServer.Client(), req, config); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer gcp-token" {
			t.Fatalf("Authorization = %q", got)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", tokenCalls.Load())
	}

	source.now = func() time.Time { return time.Date(2026, time.July, 11, 1, 7, 4, 0, time.UTC) }
	req := httptest.NewRequest(http.MethodPost, "https://vertex.example.test", nil)
	if err := source.Apply(req.Context(), tokenServer.Client(), req, config); err != nil {
		t.Fatalf("Apply() after default max TTL error = %v", err)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("token endpoint calls after default max TTL = %d, want 2", tokenCalls.Load())
	}
}

func TestGoogleTokenSourceReusesValidToken(t *testing.T) {
	var exchanges atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exchanges.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"shared-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	serviceAccount, err := json.Marshal(map[string]any{
		"type":         "service_account",
		"client_email": "shared@example.test",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"token_uri":    tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}

	source, err := NewGoogleTokenSource(
		t.Context(),
		serviceAccount,
		[]string{gcpCloudPlatformScope},
		tokenServer.Client(),
	)
	if err != nil {
		t.Fatalf("NewGoogleTokenSource() error = %v", err)
	}
	first, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != second.AccessToken || exchanges.Load() != 1 {
		t.Fatalf("tokens/exchanges = %q/%q/%d", first.AccessToken, second.AccessToken, exchanges.Load())
	}
}

func TestGCPTokenSourceRejectsMissingServiceAccount(t *testing.T) {
	t.Setenv("GCP_SERVICE_ACCOUNT", "")
	source := NewGCPTokenSource()
	if _, err := source.Token(t.Context(), http.DefaultClient, GCPConfig{}); err == nil {
		t.Fatal("Token() error = nil, want missing service account error")
	}
}

func TestGCPTokenSourceRejectsMissingServiceAccountFromConfig(t *testing.T) {
	t.Setenv("GCP_SERVICE_ACCOUNT", "")
	source := NewGCPTokenSource()
	if _, err := source.Token(t.Context(), http.DefaultClient, GCPConfig{ServiceAccountJSON: ""}); err == nil {
		t.Fatal("Token() error = nil, want missing service account error")
	}
}

func TestGCPTokenSourceFailsClosedOnTokenEndpointError(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer tokenServer.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	serviceAccount, err := json.Marshal(map[string]any{
		"client_email": "service@example.test",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"token_uri":    tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}

	source := NewGCPTokenSource()
	request := httptest.NewRequest(http.MethodPost, "https://vertex.example.test", nil)
	err = source.Apply(
		request.Context(),
		tokenServer.Client(),
		request,
		GCPConfig{ServiceAccountJSON: string(serviceAccount)},
	)
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("Apply() error = %v, want token endpoint status", err)
	}
	if authorization := request.Header.Get("Authorization"); authorization != "" {
		t.Fatalf("Authorization = %q, want no provider credential after token failure", authorization)
	}
}

func TestGCPTokenSourceCachesWithMaxTTL(t *testing.T) {
	var tokenCalls atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"cached-token","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	serviceAccount, err := json.Marshal(map[string]any{
		"type":           "service_account",
		"client_email":   "service@example.test",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"private_key_id": "key-id",
		"token_uri":      tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	source := NewGCPTokenSource()
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	source.now = func() time.Time { return now }
	config := GCPConfig{ServiceAccountJSON: string(serviceAccount), MaxTTL: 5}

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "https://vertex.example.test", nil)
		if err := source.Apply(req.Context(), tokenServer.Client(), req, config); err != nil {
			t.Fatalf("Apply() error = %v", err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token endpoint calls = %d, want 1 (cached)", tokenCalls.Load())
	}

	source.now = func() time.Time { return now.Add(6 * time.Second) }
	req := httptest.NewRequest(http.MethodPost, "https://vertex.example.test", nil)
	if err := source.Apply(req.Context(), tokenServer.Client(), req, config); err != nil {
		t.Fatalf("Apply() after TTL error = %v", err)
	}
	if tokenCalls.Load() != 2 {
		t.Fatalf("token endpoint calls after TTL = %d, want 2", tokenCalls.Load())
	}
}
