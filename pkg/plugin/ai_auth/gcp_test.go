package ai_auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestGCPServiceAccountTokenExchangeAndCacheContract(t *testing.T) {
	var calls atomic.Int64
	var tokenServer *httptest.Server
	tokenServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if got := r.Form.Get("grant_type"); got != gcpJWTBearerGrantType {
			t.Errorf("grant_type = %q, want %q", got, gcpJWTBearerGrantType)
		}
		assertion := r.Form.Get("assertion")
		parts := strings.Split(assertion, ".")
		if len(parts) != 3 || parts[2] == "" {
			t.Errorf("assertion = %q, want signed JWT", assertion)
			http.Error(w, "invalid assertion", http.StatusBadRequest)
			return
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Errorf("decode JWT payload: %v", err)
			http.Error(w, "invalid assertion", http.StatusBadRequest)
			return
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Errorf("decode JWT claims: %v", err)
			http.Error(w, "invalid assertion", http.StatusBadRequest)
			return
		}
		if claims["aud"] != tokenServer.URL || claims["scope"] != gcpCloudPlatformScope {
			t.Errorf("JWT claims = %#v, want token audience and cloud-platform scope", claims)
		}
		if issuer, _ := claims["iss"].(string); !strings.HasSuffix(issuer, "@example.test") {
			t.Errorf("JWT issuer = %#v, want service-account email", claims["iss"])
		}
		call := calls.Add(1)
		_, _ = fmt.Fprintf(w, `{"access_token":"token-%d","expires_in":3600}`, call)
	}))
	t.Cleanup(tokenServer.Close)

	baseTime := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	now := baseTime
	source := NewGCPTokenSource()
	source.now = func() time.Time { return now }
	accountOne := testServiceAccount(t, tokenServer.URL, "one@example.test")
	configuredTTL := GCPConfig{ServiceAccountJSON: string(accountOne), MaxTTL: 8}

	req := httptest.NewRequest(http.MethodPost, "https://aiplatform.example.test/predict", nil)
	if err := source.Apply(t.Context(), tokenServer.Client(), req, configuredTTL); err != nil {
		t.Fatalf("apply first access token: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token-1" {
		t.Fatalf("Authorization = %q, want Bearer token-1", got)
	}
	now = baseTime.Add(7 * time.Second)
	if token, err := source.Token(t.Context(), tokenServer.Client(), configuredTTL); err != nil || token != "token-1" {
		t.Fatalf("cached configured-TTL token = (%q, %v), want token-1", token, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token endpoint calls before configured max_ttl = %d, want 1", got)
	}
	now = baseTime.Add(9 * time.Second)
	if token, err := source.Token(t.Context(), tokenServer.Client(), configuredTTL); err != nil || token != "token-2" {
		t.Fatalf("refreshed configured-TTL token = (%q, %v), want token-2", token, err)
	}

	accountTwo := testServiceAccount(t, tokenServer.URL, "two@example.test")
	defaultTTL := GCPConfig{ServiceAccountJSON: string(accountTwo)}
	now = baseTime
	if token, err := source.Token(t.Context(), tokenServer.Client(), defaultTTL); err != nil || token != "token-3" {
		t.Fatalf("second service-account token = (%q, %v), want token-3", token, err)
	}
	now = baseTime.Add(299 * time.Second)
	if token, err := source.Token(t.Context(), tokenServer.Client(), defaultTTL); err != nil || token != "token-3" {
		t.Fatalf("cached default-TTL token = (%q, %v), want token-3", token, err)
	}
	now = baseTime.Add(301 * time.Second)
	if token, err := source.Token(t.Context(), tokenServer.Client(), defaultTTL); err != nil || token != "token-4" {
		t.Fatalf("refreshed default-TTL token = (%q, %v), want token-4", token, err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("token endpoint calls = %d, want four exchanges across configured/default TTL and two accounts", got)
	}
}

func testServiceAccount(t *testing.T, tokenURI string, email string) []byte {
	t.Helper()
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
		"client_email":   email,
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		"private_key_id": "key-id",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}
	return serviceAccount
}

func TestGCPTokenRefreshRunsSingleFlightOutsideTheCacheLock(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockingCalls atomic.Int64
	blockingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if blockingCalls.Add(1) == 1 {
			close(blocked)
			<-release
		}
		_, _ = w.Write([]byte(`{"access_token":"refreshed-token","expires_in":3600}`))
	}))
	defer blockingServer.Close()
	var fastCalls atomic.Int64
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fastCalls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"fast-token","expires_in":3600}`))
	}))
	defer fastServer.Close()

	blockedAccount := testServiceAccount(t, blockingServer.URL, "blocked@example.test")
	fastAccount := testServiceAccount(t, fastServer.URL, "fast@example.test")

	source := NewGCPTokenSource()
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	source.now = func() time.Time { return now }

	// Prime the fast key with a valid cached token.
	if _, err := source.Token(
		t.Context(),
		fastServer.Client(),
		GCPConfig{ServiceAccountJSON: string(fastAccount)},
	); err != nil {
		t.Fatalf("prime fast token: %v", err)
	}

	// Seed the blocked key with an expired token so the first caller starts
	// a refresh.
	blockedKey := sha256Hex(blockedAccount)
	source.cache[blockedKey] = cachedGCPToken{value: "stale-token", expires: now.Add(-time.Second)}

	// Caller A starts the refresh and blocks on the token endpoint.
	type tokenResult struct {
		value string
		err   error
	}
	aResult := make(chan tokenResult, 1)
	go func() {
		value, err := source.Token(
			t.Context(),
			blockingServer.Client(),
			GCPConfig{ServiceAccountJSON: string(blockedAccount)},
		)
		aResult <- tokenResult{value: value, err: err}
	}()
	<-blocked

	// A second caller for the same key must wait for the single refresh.
	bResult := make(chan tokenResult, 1)
	go func() {
		value, err := source.Token(
			t.Context(),
			blockingServer.Client(),
			GCPConfig{ServiceAccountJSON: string(blockedAccount)},
		)
		bResult <- tokenResult{value: value, err: err}
	}()
	select {
	case result := <-bResult:
		t.Fatalf("waiter returned before refresh completed: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}

	// A cancelled waiter must honor cancellation without waiting for the
	// blocked network exchange.
	cancelCtx, cancel := context.WithCancel(t.Context())
	cResult := make(chan tokenResult, 1)
	go func() {
		value, err := source.Token(
			cancelCtx,
			blockingServer.Client(),
			GCPConfig{ServiceAccountJSON: string(blockedAccount)},
		)
		cResult <- tokenResult{value: value, err: err}
	}()
	cancel()
	select {
	case result := <-cResult:
		if result.err == nil {
			t.Fatalf("cancelled waiter returned token %q, want error", result.value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter ignored cancellation")
	}

	// A cached read for a different key must continue while the refresh is
	// blocked: no mutex may be held during the network exchange.
	start := time.Now()
	fastValue, err := source.Token(t.Context(), fastServer.Client(), GCPConfig{ServiceAccountJSON: string(fastAccount)})
	if err != nil || fastValue != "fast-token" {
		t.Fatalf("cached read = (%q, %v), want fast-token", fastValue, err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("cached read waited %s while another key refreshed", elapsed)
	}

	close(release)
	for range 2 {
		select {
		case result := <-aResult:
			if result.err != nil || result.value != "refreshed-token" {
				t.Fatalf("refresher = (%q, %v), want refreshed-token", result.value, result.err)
			}
		case result := <-bResult:
			if result.err != nil || result.value != "refreshed-token" {
				t.Fatalf("waiter = (%q, %v), want refreshed-token", result.value, result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("refresh result never published")
		}
	}
	if got := blockingCalls.Load(); got != 1 {
		t.Fatalf("blocking token endpoint calls = %d, want exactly 1 refresh", got)
	}
}

func TestGCPTokenRefreshFailureReturnsErrorAfterExpiryAndRetainsEntry(t *testing.T) {
	var calls atomic.Int64
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"access_token":"first-token","expires_in":3600}`))
			return
		}
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer tokenServer.Close()
	account := testServiceAccount(t, tokenServer.URL, "retry@example.test")

	source := NewGCPTokenSource()
	now := time.Date(2026, time.July, 11, 1, 2, 3, 0, time.UTC)
	source.now = func() time.Time { return now }
	config := GCPConfig{ServiceAccountJSON: string(account)}

	first, err := source.Token(t.Context(), tokenServer.Client(), config)
	if err != nil || first != "first-token" {
		t.Fatalf("first token = (%q, %v)", first, err)
	}

	// After the cached token expires (max TTL is 300s), a failed refresh
	// surfaces the error while the previous entry stays cached so it remains
	// readable.
	source.now = func() time.Time { return now.Add(6 * time.Minute) }
	if value, err := source.Token(t.Context(), tokenServer.Client(), config); err == nil ||
		!strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("expired refresh = (%q, %v), want token endpoint rejection", value, err)
	}
	cached, ok := source.cache[sha256Hex(account)]
	if !ok || cached.value != "first-token" {
		t.Fatalf("cache entry after failed refresh = %#v, want retained first-token", cached)
	}
}
