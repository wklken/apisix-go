package jwe_decrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type jweConsumerLookup struct {
	mu     sync.RWMutex
	byKey  map[string]resource.Consumer
	calls  []string
	closed bool
}

func (lookup *jweConsumerLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.calls = append(lookup.calls, plugin+"\x00"+key)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (*jweConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*jweConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *jweConsumerLookup) close() {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.closed = true
	lookup.byKey = nil
}

func jweBoundConsumer(username, key, secret string, encoded bool) resource.Consumer {
	return resource.Consumer{
		Username: username,
		Plugins: map[string]resource.PluginConfig{name: map[string]any{
			"key": key, "secret": secret, "is_base64_encoded": encoded,
		}},
	}
}

func newLookupTestPlugin(t *testing.T, cfg Config, lookup base.ConsumerLookup) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

type jweTestFixture struct {
	sync.Mutex
	consumers []resource.Consumer
}

var jweTestFixtures sync.Map

func jweFixtureFor(t *testing.T) *jweTestFixture {
	t.Helper()
	fixture := &jweTestFixture{}
	actual, loaded := jweTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { jweTestFixtures.Delete(t) })
	}
	return actual.(*jweTestFixture)
}

func addJWEConsumer(t *testing.T, username, key, secret string, base64Encoded bool) {
	t.Helper()
	fixture := jweFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(
		fixture.consumers,
		jweBoundConsumer(username, key, secret, base64Encoded),
	)
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	fixture := jweFixtureFor(t)
	fixture.Lock()
	consumers := append([]resource.Consumer(nil), fixture.consumers...)
	fixture.Unlock()
	lookup := &jweConsumerLookup{byKey: make(map[string]resource.Consumer, len(consumers))}
	for _, consumer := range consumers {
		config, ok := consumer.Plugins[name].(map[string]any)
		if !ok {
			continue
		}
		key, ok := config["key"].(string)
		if ok {
			lookup.byKey[key] = consumer
		}
	}

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerDecryptsBearerJWEAndForwardsPlaintext(t *testing.T) {
	secret := "12345678901234567890123456789012"
	addJWEConsumer(t, "jwe-user", "kid-1", secret, false)
	p := newTestPlugin(t, Config{ForwardHeader: "X-Forwarded-Authorization"})
	token := makeCompactJWE(t, "kid-1", []byte(secret), "Bearer upstream-token")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Forwarded-Authorization"); got != "Bearer upstream-token" {
			t.Fatalf("forward header = %q, want plaintext", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerUsesInjectedJWEConsumerLookupAuthoritatively(t *testing.T) {
	const (
		kid           = "lookup-kid"
		unknownSecret = "12345678901234567890123456789012"
		lookupSecret  = "abcdefghijklmnopqrstuvwxyz123456"
	)
	lookup := &jweConsumerLookup{byKey: map[string]resource.Consumer{
		kid: jweBoundConsumer("lookup-jwe-user", kid, lookupSecret, false),
	}}
	p := newLookupTestPlugin(t, Config{ForwardHeader: "X-Decrypted"}, lookup)
	token := makeCompactJWE(t, kid, []byte(lookupSecret), "Bearer lookup-plaintext")
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Decrypted"); got != "Bearer lookup-plaintext" {
			t.Fatalf("forwarded plaintext = %q", got)
		}
		state, ok := ctx.AuthenticationStateFrom(r)
		if !ok || state.Consumer().Username != "lookup-jwe-user" {
			t.Fatalf("authentication state = %#v/%v", state, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", response.Code, response.Body.String())
	}
	lookup.mu.RLock()
	if len(lookup.calls) != 1 || lookup.calls[0] != name+"\x00"+kid {
		t.Fatalf("lookup calls = %#v, want exact factory/kid", lookup.calls)
	}
	lookup.mu.RUnlock()

	miss := newLookupTestPlugin(t, Config{}, &jweConsumerLookup{})
	unknownToken := makeCompactJWE(t, kid, []byte(unknownSecret), "unknown-plaintext")
	missRequest := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	missRequest.Header.Set("Authorization", unknownToken)
	missResponse := httptest.NewRecorder()
	miss.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("JWE lookup miss reached downstream")
	})).ServeHTTP(missResponse, missRequest)
	if missResponse.Code != http.StatusBadRequest ||
		!strings.Contains(missResponse.Body.String(), "invalid kid in JWE token") {
		t.Fatalf(
			"lookup miss response = %d/%q, want exact invalid-kid 400",
			missResponse.Code,
			missResponse.Body.String(),
		)
	}
}

func TestJWEConsumerLookupsAreGenerationIsolated(t *testing.T) {
	const (
		kid          = "overlap-kid"
		firstSecret  = "12345678901234567890123456789012"
		secondSecret = "abcdefghijklmnopqrstuvwxyz123456"
	)
	firstLookup := &jweConsumerLookup{byKey: map[string]resource.Consumer{
		kid: jweBoundConsumer("jwe-generation-n", kid, firstSecret, false),
	}}
	secondLookup := &jweConsumerLookup{byKey: map[string]resource.Consumer{
		kid: jweBoundConsumer("jwe-generation-n-plus-one", kid, secondSecret, false),
	}}
	first := newLookupTestPlugin(t, Config{ForwardHeader: "X-Decrypted"}, firstLookup)
	second := newLookupTestPlugin(t, Config{ForwardHeader: "X-Decrypted"}, secondLookup)
	assertConsumer := func(p *Plugin, secret, plaintext, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request.Header.Set("Authorization", makeCompactJWE(t, kid, []byte(secret), plaintext))
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state, ok := ctx.AuthenticationStateFrom(r)
			if !ok || state.Consumer().Username != want || r.Header.Get("X-Decrypted") != plaintext {
				t.Errorf("generation result = %#v/%v/%q", state, ok, r.Header.Get("X-Decrypted"))
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", response.Code)
		}
	}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { assertConsumer(first, firstSecret, "first", "jwe-generation-n") })
		group.Go(func() {
			assertConsumer(second, secondSecret, "second", "jwe-generation-n-plus-one")
		})
	}
	group.Wait()
	firstLookup.close()
	assertConsumer(second, secondSecret, "second-after-close", "jwe-generation-n-plus-one")
}

func TestHandlerDecryptsBase64EncodedConsumerSecret(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz123456")
	encodedSecret := base64.RawURLEncoding.EncodeToString(secret)
	addJWEConsumer(t, "jwe-base64-user", "kid-base64", encodedSecret, true)
	p := newTestPlugin(t, Config{})
	token := makeCompactJWE(t, "kid-base64", secret, "Bearer base64-secret")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer base64-secret" {
			t.Fatalf("Authorization header = %q, want decrypted plaintext", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerDecryptsStdBase64EncodedConsumerSecret(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz123456")
	encodedSecret := base64.StdEncoding.EncodeToString(secret)
	addJWEConsumer(t, "jwe-std-base64-user", "kid-std-base64", encodedSecret, true)
	p := newTestPlugin(t, Config{})
	token := makeCompactJWE(t, "kid-std-base64", secret, "std-base64-secret")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "std-base64-secret" {
			t.Fatalf("Authorization header = %q, want decrypted plaintext", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerSelectsConsumerByKidAndIgnoresUnsupportedDeclarations(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz123456")
	key := "kid-ignored-declarations"
	addJWEConsumer(t, "jwe-ignored-declarations-user", key, string(secret), false)
	p := newTestPlugin(t, Config{})
	token := makeCompactJWEWithDeclarations(t, map[string]any{
		"alg": "RSA-OAEP",
		"enc": "A128CBC-HS256",
		"kid": key,
	}, "not-a-base64-encrypted-key", secret, "declarations-ignored")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "declarations-ignored" {
			t.Fatalf("Authorization header = %q, want decrypted plaintext", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsMissingTokenWhenStrict(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing JWE token in request") {
		t.Fatalf("body = %q, want missing token message", rr.Body.String())
	}
}

func TestHandlerNonStrictMissingTokenPreservesNestedHandlerCompatibility(t *testing.T) {
	strict := false
	p := newTestPlugin(t, Config{Strict: &strict})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()
	called := 0

	nested := http.HandlerFunc(func(w http.ResponseWriter, got *http.Request) {
		called++
		if got != req {
			t.Fatalf("nested handler request = %p, want original %p", got, req)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	p.Handler(nested).ServeHTTP(rr, req)

	if called != 1 {
		t.Fatalf("nested handler calls = %d, want 1", called)
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsInvalidToken(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", "not-a-jwe")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "JWE token invalid") {
		t.Fatalf("body = %q, want invalid token message", rr.Body.String())
	}
}

func TestHandlerRejectsUnknownKid(t *testing.T) {
	token := makeCompactJWE(t, "unknown-kid", []byte("12345678901234567890123456789012"), "Bearer upstream-token")
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid kid in JWE token") {
		t.Fatalf("body = %q, want invalid kid message", rr.Body.String())
	}
}

func TestDecryptJWERejectsConsumerSecretThatIsNot32Bytes(t *testing.T) {
	secret := []byte("1234567890123456")
	token := makeCompactJWE(t, "kid-short-secret", secret, "Bearer upstream-token")
	parsed, err := parseCompactJWE(token)
	if err != nil {
		t.Fatalf("parseCompactJWE() error = %v", err)
	}

	_, err = decryptJWE(parsed, map[string]any{"secret": string(secret)})
	if err == nil {
		t.Fatal("decryptJWE() error = nil, want 32-byte secret validation error")
	}
}

func makeCompactJWE(t *testing.T, kid string, secret []byte, plaintext string) string {
	t.Helper()

	return makeCompactJWEWithDeclarations(t, map[string]any{
		"alg": "dir",
		"enc": "A256GCM",
		"kid": kid,
	}, "", secret, plaintext)
}

func makeCompactJWEWithDeclarations(
	t *testing.T,
	headerValue map[string]any,
	encryptedKey string,
	secret []byte,
	plaintext string,
) string {
	t.Helper()

	header, err := json.Marshal(headerValue)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	protectedHeader := base64.RawURLEncoding.EncodeToString(header)
	iv := []byte("123456789012")

	block, err := aes.NewCipher(secret)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new gcm: %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagStart := len(sealed) - gcm.Overhead()

	return strings.Join([]string{
		protectedHeader,
		encryptedKey,
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(sealed[:tagStart]),
		base64.RawURLEncoding.EncodeToString(sealed[tagStart:]),
	}, ".")
}
