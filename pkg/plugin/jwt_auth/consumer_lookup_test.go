package jwt_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func newJWTConsumerLookup(
	t *testing.T,
	consumers []runtime.ConsumerRecord,
	credentials []runtime.ConsumerCredentialBinding,
) *runtime.ConsumerBindings {
	t.Helper()
	lookup, err := runtime.NewConsumerBindings(consumers, nil, credentials)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	t.Cleanup(lookup.Close)
	return lookup
}

func jwtConsumerRecord(id, username string, config map[string]any) runtime.ConsumerRecord {
	plugins := map[string]resource.PluginConfig{}
	if config != nil {
		plugins[name] = config
	}
	return runtime.ConsumerRecord{
		ID: id,
		Consumer: resource.Consumer{
			Username: username,
			Plugins:  plugins,
		},
	}
}

func bindJWTConsumer(id, key string) runtime.ConsumerCredentialBinding {
	return runtime.ConsumerCredentialBinding{Plugin: name, Key: key, ConsumerID: id}
}

func TestJWTConsumerLookupUsesResolvedBase64SecretClaimsAndGracePeriod(t *testing.T) {
	const key = "jwt-lookup-resolved-key"
	addJWTConsumer(t, "jwt-store-poison-resolved", key, "store-poison-secret")
	resolvedSecret := "resolved-private-secret"
	lookup := newJWTConsumerLookup(t,
		[]runtime.ConsumerRecord{jwtConsumerRecord("jwt-resolved", "jwt-resolved-user", map[string]any{
			"key":                   key,
			"secret":                base64.StdEncoding.EncodeToString([]byte(resolvedSecret)),
			"algorithm":             "HS256",
			"base64_secret":         true,
			"lifetime_grace_period": 120,
		})},
		[]runtime.ConsumerCredentialBinding{bindJWTConsumer("jwt-resolved", key)},
	)
	p := newTestPlugin(t, Config{ClaimsToVerify: []string{"exp"}})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	token := signHS256(t, resolvedSecret, map[string]any{
		"key": key,
		"exp": time.Now().Add(-60 * time.Second).Unix(),
	})

	response := performRequest(p.Handler(assertConsumer(t, "jwt-resolved-user")), token)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestJWTConsumerLookupUsesResolvedAsymmetricPublicKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const key = "jwt-lookup-rsa-key"
	lookup := newJWTConsumerLookup(t,
		[]runtime.ConsumerRecord{jwtConsumerRecord("jwt-rsa", "jwt-rsa-user", map[string]any{
			"key":        key,
			"algorithm":  "RS256",
			"public_key": publicKeyPEM(t, &privateKey.PublicKey),
		})},
		[]runtime.ConsumerCredentialBinding{bindJWTConsumer("jwt-rsa", key)},
	)
	p := newTestPlugin(t, Config{})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	token := signRS256(t, privateKey, map[string]any{
		"key": key,
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	response := performRequest(p.Handler(assertConsumer(t, "jwt-rsa-user")), token)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestJWTConsumerLookupMissDoesNotFallBackToStore(t *testing.T) {
	const key = "jwt-authoritative-miss-key"
	addJWTConsumer(t, "jwt-store-poison-miss", key, "store-only-secret")
	lookup := newJWTConsumerLookup(t, nil, nil)
	p := newTestPlugin(t, Config{})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	token := signHS256(t, "store-only-secret", map[string]any{
		"key": key,
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	response := performRequest(p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("lookup miss reached Store-backed consumer")
	})), token)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestJWTAnonymousLookupMissDoesNotFallBackToStore(t *testing.T) {
	const anonymousID = "jwt-store-only-anonymous"
	addConsumer(t, anonymousID)
	lookup := newJWTConsumerLookup(t, nil, nil)
	p := newTestPlugin(t, Config{AnonymousConsumer: anonymousID})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("anonymous lookup miss reached Store-backed consumer")
	})).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestJWTAnonymousConsumerUsesLookupByID(t *testing.T) {
	const anonymousID = "jwt-lookup-anonymous"
	lookup := newJWTConsumerLookup(t,
		[]runtime.ConsumerRecord{jwtConsumerRecord(anonymousID, "jwt-lookup-anonymous-user", nil)},
		nil,
	)
	p := newTestPlugin(t, Config{AnonymousConsumer: anonymousID})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	response := httptest.NewRecorder()

	p.Handler(assertConsumer(t, "jwt-lookup-anonymous-user")).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestJWTConsumerLookupsAreAttemptIsolatedAndClosingNPreservesNPlusOne(t *testing.T) {
	const key = "jwt-overlap-key"
	addJWTConsumer(t, "jwt-overlap-store-poison", key, "store-poison-secret")
	firstLookup := newJWTConsumerLookup(t,
		[]runtime.ConsumerRecord{jwtConsumerRecord("jwt-n", "jwt-user-n", map[string]any{
			"key": key, "secret": "jwt-secret-n", "algorithm": "HS256",
		})},
		[]runtime.ConsumerCredentialBinding{bindJWTConsumer("jwt-n", key)},
	)
	secondLookup := newJWTConsumerLookup(t,
		[]runtime.ConsumerRecord{jwtConsumerRecord("jwt-n-plus-one", "jwt-user-n-plus-one", map[string]any{
			"key": key, "secret": "jwt-secret-n-plus-one", "algorithm": "HS256",
		})},
		[]runtime.ConsumerCredentialBinding{bindJWTConsumer("jwt-n-plus-one", key)},
	)
	first := newTestPlugin(t, Config{})
	first.SetDependencies(base.Dependencies{Consumers: firstLookup})
	second := newTestPlugin(t, Config{})
	second.SetDependencies(base.Dependencies{Consumers: secondLookup})
	firstToken := signHS256(t, "jwt-secret-n", map[string]any{"key": key})
	secondToken := signHS256(t, "jwt-secret-n-plus-one", map[string]any{"key": key})

	if response := performRequest(
		first.Handler(assertConsumer(t, "jwt-user-n")),
		firstToken,
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N response code = %d; body=%s", response.Code, response.Body.String())
	}
	if response := performRequest(
		second.Handler(assertConsumer(t, "jwt-user-n-plus-one")),
		secondToken,
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N+1 response code = %d; body=%s", response.Code, response.Body.String())
	}

	firstLookup.Close()
	if response := performRequest(first.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("closed generation N reached downstream")
	})), firstToken); response.Code != http.StatusUnauthorized {
		t.Fatalf("closed generation N response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response := performRequest(
		second.Handler(assertConsumer(t, "jwt-user-n-plus-one")),
		secondToken,
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N+1 after closing N response code = %d; body=%s", response.Code, response.Body.String())
	}
}
