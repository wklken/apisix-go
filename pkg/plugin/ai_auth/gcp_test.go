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
}

func TestGCPTokenSourceRejectsMissingServiceAccount(t *testing.T) {
	t.Setenv("GCP_SERVICE_ACCOUNT", "")
	source := NewGCPTokenSource()
	if _, err := source.Token(t.Context(), http.DefaultClient, GCPConfig{}); err == nil {
		t.Fatal("Token() error = nil, want missing service account error")
	}
}
