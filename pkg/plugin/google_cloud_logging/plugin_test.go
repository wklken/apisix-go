package google_cloud_logging

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestPostInitSetsGoogleDefaults(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
	})

	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.Resource.Type != "global" {
		t.Fatalf("resource.type = %q, want global", p.config.Resource.Type)
	}
	if p.config.LogID != "apisix.apache.org%2Flogs" {
		t.Fatalf("log_id = %q, want apisix.apache.org%%2Flogs", p.config.LogID)
	}
	if p.config.AuthConfig.EntriesURI != "https://logging.googleapis.com/v2/entries:write" {
		t.Fatalf("entries_uri = %q, want default Google entries endpoint", p.config.AuthConfig.EntriesURI)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestBuildJWTAssertionUsesServiceAccountClaims(t *testing.T) {
	pemKey, key := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
			Scopes:      []string{"scope-a", "scope-b"},
		},
	})

	assertion, err := p.buildJWTAssertion(time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("buildJWTAssertion() error = %v", err)
	}

	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts, want 3", len(parts))
	}

	var header map[string]any
	mustDecodeJWTPart(t, parts[0], &header)
	if header["alg"] != "RS256" {
		t.Fatalf("alg = %v, want RS256", header["alg"])
	}

	var claims map[string]any
	mustDecodeJWTPart(t, parts[1], &claims)
	if claims["iss"] != "svc@example.iam.gserviceaccount.com" {
		t.Fatalf("iss = %v, want service account email", claims["iss"])
	}
	if claims["sub"] != "svc@example.iam.gserviceaccount.com" {
		t.Fatalf("sub = %v, want service account email", claims["sub"])
	}
	if claims["aud"] != "http://127.0.0.1/token" {
		t.Fatalf("aud = %v, want token uri", claims["aud"])
	}
	if claims["scope"] != "scope-a scope-b" {
		t.Fatalf("scope = %v, want joined scopes", claims["scope"])
	}

	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatalf("verify jwt signature: %v", err)
	}
}

func TestBuildEntryUsesCloudLoggingShape(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
		Resource: MonitoredResource{
			Type:   "global",
			Labels: map[string]string{"project_id": "project-a"},
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	entry := p.buildEntry(map[string]any{"path": "/orders"})
	if entry.LogName != "projects/project-a/logs/apisix.apache.org%2Flogs" {
		t.Fatalf("logName = %q, want project log name", entry.LogName)
	}
	if entry.Labels["source"] != "apache-apisix-google-cloud-logging" {
		t.Fatalf("source label = %q, want apache source label", entry.Labels["source"])
	}
	if entry.Resource.Type != "global" {
		t.Fatalf("resource.type = %q, want global", entry.Resource.Type)
	}
	if entry.JSONPayload["path"] != "/orders" {
		t.Fatalf("jsonPayload path = %v, want /orders", entry.JSONPayload["path"])
	}
	if entry.Timestamp == "" {
		t.Fatal("timestamp is empty")
	}
}

func TestHandlerBuildsDefaultHTTPRequestEntry(t *testing.T) {
	p := &Plugin{config: Config{
		AuthConfig: &AuthConfig{
			ProjectID: "project-a",
		},
		Resource: MonitoredResource{Type: "global"},
		LogID:    defaultLogID,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders?debug=true", strings.NewReader("payload"))
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("User-Agent", "apisix-go-test")
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":   "route-1",
		"$service_id": "service-1",
	})
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	var fields map[string]any
	select {
	case fields = <-p.FireChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for google log fields")
	}

	entry := p.buildEntry(fields)
	if entry.HTTPRequest == nil {
		t.Fatal("httpRequest is nil")
	}
	if entry.HTTPRequest.RequestMethod != http.MethodPost {
		t.Fatalf("requestMethod = %q, want POST", entry.HTTPRequest.RequestMethod)
	}
	if entry.HTTPRequest.RequestURL != "http://example.com/orders?debug=true" {
		t.Fatalf("requestUrl = %q, want full request URL", entry.HTTPRequest.RequestURL)
	}
	if entry.HTTPRequest.RequestSize != 7 {
		t.Fatalf("requestSize = %d, want 7", entry.HTTPRequest.RequestSize)
	}
	if entry.HTTPRequest.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", entry.HTTPRequest.Status)
	}
	if entry.HTTPRequest.ResponseSize != 7 {
		t.Fatalf("responseSize = %d, want 7", entry.HTTPRequest.ResponseSize)
	}
	if entry.HTTPRequest.UserAgent != "apisix-go-test" {
		t.Fatalf("userAgent = %q, want apisix-go-test", entry.HTTPRequest.UserAgent)
	}
	if entry.HTTPRequest.RemoteIP != "203.0.113.10" {
		t.Fatalf("remoteIp = %q, want 203.0.113.10", entry.HTTPRequest.RemoteIP)
	}
	if entry.HTTPRequest.Latency == "" {
		t.Fatal("latency is empty")
	}
	if entry.JSONPayload["route_id"] != "route-1" {
		t.Fatalf("route_id = %v, want route-1", entry.JSONPayload["route_id"])
	}
	if entry.JSONPayload["service_id"] != "service-1" {
		t.Fatalf("service_id = %v, want service-1", entry.JSONPayload["service_id"])
	}
}

func TestBuildEntryKeepsCustomLogFormatInJSONPayload(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	entry := p.buildEntry(map[string]any{"path": "/orders"})
	if entry.HTTPRequest != nil {
		t.Fatalf("httpRequest = %#v, want nil for custom log_format", entry.HTTPRequest)
	}
	if entry.JSONPayload["path"] != "/orders" {
		t.Fatalf("jsonPayload path = %v, want /orders", entry.JSONPayload["path"])
	}
}

func TestSendExchangesTokenAndWritesEntries(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryRequests := make(chan *http.Request, 1)
	entryBodies := make(chan map[string]any, 1)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode entries body: %v", err)
		}
		entryRequests <- r
		entryBodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		SSLVerify: &sslVerify,
		LogFormat: map[string]string{"path": "$uri"},
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case form := <-tokenRequests:
		if form.Get("grant_type") != jwtBearerGrantType {
			t.Fatalf("grant_type = %q, want jwt bearer grant", form.Get("grant_type"))
		}
		if form.Get("assertion") == "" {
			t.Fatal("assertion is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for token request")
	}

	select {
	case req := <-entryRequests:
		if got := req.Header.Get("Authorization"); got != "Bearer token-a" {
			t.Fatalf("Authorization = %q, want Bearer token-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entries request")
	}

	select {
	case body := <-entryBodies:
		if body["partialSuccess"] != false {
			t.Fatalf("partialSuccess = %v, want false", body["partialSuccess"])
		}
		entries, ok := body["entries"].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("entries = %#v, want one entry", body["entries"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entries body")
	}
}

func TestSendBatchWritesGoogleEntries(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryBodies := make(chan map[string]any, 1)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode entries body: %v", err)
		}
		entryBodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-entryBodies:
		entries, ok := body["entries"].([]any)
		if !ok {
			t.Fatalf("entries = %#v, want array", body["entries"])
		}
		if len(entries) != 2 {
			t.Fatalf("entries length = %d, want 2", len(entries))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Google Cloud Logging entries request")
	}

	select {
	case <-tokenRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Google OAuth token request")
	}
}

func TestSendBatchReusesCachedAccessToken(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 2)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryRequests := make(chan string, 2)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entryRequests <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}}, 1); err != nil {
		t.Fatalf("first SendBatch() error = %v", err)
	}
	if _, err := p.SendBatch([]map[string]any{{"path": "/b"}}, 1); err != nil {
		t.Fatalf("second SendBatch() error = %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case auth := <-entryRequests:
			if auth != "Bearer token-a" {
				t.Fatalf("Authorization = %q, want cached Bearer token-a", auth)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for entries request")
		}
	}

	select {
	case <-tokenRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first token request")
	}
	select {
	case extra := <-tokenRequests:
		t.Fatalf("unexpected second token request: %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSendBatchRefreshesExpiredAccessToken(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 2)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":30}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}}, 1); err != nil {
		t.Fatalf("first SendBatch() error = %v", err)
	}
	if _, err := p.SendBatch([]map[string]any{{"path": "/b"}}, 1); err != nil {
		t.Fatalf("second SendBatch() error = %v", err)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-tokenRequests:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for token request %d", i+1)
		}
	}
}

func testPrivateKey(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: mustMarshalPKCS8(t, key),
	})
	return string(pemKey), key
}

func mustMarshalPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()

	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8 key: %v", err)
	}
	return encoded
}

func mustDecodeJWTPart(t *testing.T, part string, v any) {
	t.Helper()

	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode jwt part: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal jwt part: %v", err)
	}
}

func TestLoadAuthConfigFromFile(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	file := writeTempAuthFile(t, pemKey)
	p := newTestPlugin(t, Config{AuthFile: file})

	auth, err := p.authConfig()
	if err != nil {
		t.Fatalf("authConfig() error = %v", err)
	}
	if auth.ProjectID != "project-from-file" {
		t.Fatalf("project_id = %q, want project-from-file", auth.ProjectID)
	}
}

func writeTempAuthFile(t *testing.T, pemKey string) string {
	t.Helper()

	body := map[string]any{
		"client_email": "svc@example.iam.gserviceaccount.com",
		"private_key":  pemKey,
		"project_id":   "project-from-file",
		"token_uri":    "http://127.0.0.1/token",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}

	file := t.TempDir() + "/auth.json"
	if err := writeFile(file, data); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return file
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
