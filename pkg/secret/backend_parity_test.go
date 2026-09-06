package secret

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestParityAWSSecretManagerDispatchAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" ||
			!strings.Contains(r.Header.Get("Authorization"), "/secretsmanager/aws4_request") {
			t.Error("AWS request was not signed for GetSecretValue")
		}
		if r.Header.Get("X-Amz-Security-Token") != "synthetic-session" {
			t.Error("session token not resolved")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["SecretId"] != "credentials" || body["VersionStage"] != "AWSCURRENT" {
			t.Errorf("request body=%v", body)
		}
		_, _ = w.Write([]byte(`{"SecretString":"{\"password\":\"synthetic-result\"}"}`))
	}))
	defer server.Close()
	t.Setenv("APISIX_TEST_AWS_TOKEN", "synthetic-session")
	config, _ := json.Marshal(
		map[string]any{
			"access_key_id":     "synthetic-id",
			"secret_access_key": "synthetic-key",
			"session_token":     "$ENV://APISIX_TEST_AWS_TOKEN",
			"endpoint_url":      server.URL + "/ignored",
		},
	)
	view, scope := openGenerationResolverView(t, config, nil, "aws")
	for range 2 {
		got, err := view.ResolveReference(context.Background(), scope, "$secret://aws/test1/credentials/password")
		if err != nil || got != "synthetic-result" {
			t.Fatalf("AWS secret=%q,%v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("AWS calls=%d, want one cached fetch", calls.Load())
	}
	got, err := view.ResolveReference(context.Background(), scope, "$secret://aws/test1/credentials")
	if err != nil || got != `{"password":"synthetic-result"}` {
		t.Fatalf("whole AWS secret=%q,%v", got, err)
	}
}

func TestParityGCPSecretManagerDispatchAndCache(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	)
	var tokens, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			tokens.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" ||
				len(strings.Split(r.Form.Get("assertion"), ".")) != 3 {
				t.Error("missing signed JWT assertion")
			}
			_, _ = w.Write([]byte(`{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		requests.Add(1)
		if r.URL.Path != "/v1/projects/project/secrets/credentials/versions/latest:access" ||
			r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("invalid secret access request")
		}
		_ = json.NewEncoder(w).
			Encode(map[string]any{"payload": map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(`{"password":"synthetic-result"}`))}})
	}))
	defer server.Close()
	config, _ := json.Marshal(
		map[string]any{
			"auth_config": map[string]any{
				"client_email": "service@example.test",
				"private_key":  privateKey,
				"project_id":   "project",
				"token_uri":    server.URL + "/token",
				"entries_uri":  server.URL + "/v1",
			},
		},
	)
	view, scope := openGenerationResolverView(t, config, nil, "gcp")
	for range 2 {
		got, err := view.ResolveReference(context.Background(), scope, "$secret://gcp/test1/credentials/password")
		if err != nil || got != "synthetic-result" {
			t.Fatalf("GCP secret=%q,%v", got, err)
		}
	}
	if requests.Load() != 1 || tokens.Load() != 1 {
		t.Fatalf("requests=%d tokens=%d, want cached secret", requests.Load(), tokens.Load())
	}
}

func TestParityVaultDecodesNonSuccessBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"data":{"password":"synthetic-result"}}`))
	}))
	defer server.Close()
	view, scope := openGenerationResolverView(t, vaultConfigBytesForResolver(t, server.URL, "synthetic", ""), nil)
	got, err := view.ResolveReference(context.Background(), scope, "$secret://vault/test1/credentials/password")
	if err != nil || got != "synthetic-result" {
		t.Fatalf("Vault secret=%q,%v", got, err)
	}
}
