package hmac_auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/hmac-auth.db", testEvents)
		s.Start()
	})
}

func addHMACConsumer(t *testing.T, username, keyID, secretKey string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"hmac-auth": map[string]any{
				"key_id":     keyID,
				"secret_key": secretKey,
			},
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey("hmac-auth", keyID); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for hmac-auth key %q", username, keyID)
}

func addConsumer(t *testing.T, username string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins":  map[string]any{},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumer(username); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not stored", username)
}

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

func TestHandlerAcceptsSignedDateAndAttachesConsumer(t *testing.T) {
	addHMACConsumer(t, "hmac-user", "hmac-key", "hmac-secret")
	p := newTestPlugin(t, Config{})
	date := time.Now().UTC().Format(http.TimeFormat)
	auth := signatureHeader(t, "hmac-key", "hmac-secret", "hmac-sha256", []string{"date"}, map[string]string{
		"date": date,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-user" {
			t.Fatalf("consumer_name = %v, want hmac-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRunsConsumerPluginsAfterAuthentication(t *testing.T) {
	addHMACConsumer(t, "consumer-plugin-hmac-user", "consumer-plugin-hmac-key", "hmac-secret")
	p := newTestPlugin(t, Config{})
	date := time.Now().UTC().Format(http.TimeFormat)
	auth := signatureHeader(
		t,
		"consumer-plugin-hmac-key",
		"hmac-secret",
		"hmac-sha256",
		[]string{"date"},
		map[string]string{
			"date": date,
		},
	)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "consumer-plugin-hmac-user" {
			t.Fatalf("consumer_name = %v, want consumer-plugin-hmac-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	response := httptest.NewRecorder()
	nextCalled := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, req)

	if nextCalled {
		t.Fatal("next handler was called instead of the consumer plugin runner")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestHandlerRejectsStaleDate(t *testing.T) {
	addHMACConsumer(t, "stale-user", "stale-key", "hmac-secret")
	p := newTestPlugin(t, Config{})
	date := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	auth := signatureHeader(t, "stale-key", "hmac-secret", "hmac-sha256", []string{"date"}, map[string]string{
		"date": date,
	})

	res := performSignedRequest(t, p, auth, date, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if got := res.Header().Get("WWW-Authenticate"); got != `hmac realm="hmac"` {
		t.Fatalf("WWW-Authenticate = %q, want hmac realm", got)
	}
}

func TestHandlerWritesExactMissingAuthorizationResponse(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Body.String(); got != `{"message":"client request can't be validated: missing Authorization header"}` {
		t.Fatalf("response body = %q", got)
	}
	if got := response.Header().Get("WWW-Authenticate"); got != `hmac realm="hmac"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

func TestHandlerWritesExactMissingAnonymousConsumerResponse(t *testing.T) {
	setupStore(t)
	p := newTestPlugin(t, Config{AnonymousConsumer: "missing-consumer"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got := response.Body.String(); got != `{"message":"Invalid user authorization"}` {
		t.Fatalf("response body = %q", got)
	}
	if got := response.Header().Get("WWW-Authenticate"); got != `hmac realm="hmac"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
}

func TestHandlerUsesAnonymousConsumerWhenAuthorizationIsMissing(t *testing.T) {
	addConsumer(t, "anonymous-hmac-user")
	p := newTestPlugin(t, Config{AnonymousConsumer: "anonymous-hmac-user"})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-hmac-user" {
			t.Fatalf("consumer_name = %v, want anonymous-hmac-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerUsesAnonymousConsumerForInvalidSignatureAndHidesCredentials(t *testing.T) {
	addHMACConsumer(t, "bad-signature-hmac-user", "bad-hmac-key", "hmac-secret")
	addConsumer(t, "anonymous-bad-hmac-user")
	hideCredentials := true
	p := newTestPlugin(t, Config{
		HideCredentials:   &hideCredentials,
		AnonymousConsumer: "anonymous-bad-hmac-user",
	})
	date := time.Now().UTC().Format(http.TimeFormat)
	auth := signatureHeader(t, "bad-hmac-key", "wrong-secret", "hmac-sha256", []string{"date"}, map[string]string{
		"date": date,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-bad-hmac-user" {
			t.Fatalf("consumer_name = %v, want anonymous-bad-hmac-user", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerValidatesRequestBodyDigestAndRestoresBody(t *testing.T) {
	addHMACConsumer(t, "body-user", "body-key", "hmac-secret")
	p := newTestPlugin(t, Config{ValidateRequestBody: true})
	date := time.Now().UTC().Format(http.TimeFormat)
	body := "payload"
	digest := bodyDigest(body)
	auth := signatureHeader(t, "body-key", "hmac-secret", "hmac-sha256", []string{"date", "digest"}, map[string]string{
		"date":   date,
		"digest": digest,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/get", strings.NewReader(body))
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Digest", digest)
	req.Header.Set("Authorization", auth)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("upstream body = %q, want %q", string(got), body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerValidatesRequestTargetOnlySignature(t *testing.T) {
	addHMACConsumer(t, "target-user", "my-access-key", "my-secret-key")
	p := newTestPlugin(t, Config{SignedHeaders: []string{}, ClockSkew: 1_000_000_000})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", "Thu, 24 Sep 2020 06:39:52 GMT")
	params := signatureParams{
		KeyID:     "my-access-key",
		Algorithm: "hmac-sha256",
		Headers:   []string{"@request-target"},
		Signature: "",
	}
	generated, err := generateSignature(req, "my-secret-key", params)
	if err != nil {
		t.Fatalf("generateSignature() error = %v", err)
	}
	req.Header.Set(
		"Authorization",
		`Signature keyId="my-access-key",algorithm="hmac-sha256",headers="@request-target",signature="`+base64.StdEncoding.EncodeToString(
			generated,
		)+`"`,
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeader(t *testing.T) {
	addHMACConsumer(t, "hide-user", "hide-hmac-key", "hmac-secret")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})
	date := time.Now().UTC().Format(http.TimeFormat)
	auth := signatureHeader(t, "hide-hmac-key", "hmac-secret", "hmac-sha256", []string{"date"}, map[string]string{
		"date": date,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func performSignedRequest(t *testing.T, p *Plugin, auth, date string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", body)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)
	return rr
}

func signatureHeader(
	t *testing.T,
	keyID string,
	secret string,
	algorithm string,
	signedHeaders []string,
	values map[string]string,
) string {
	t.Helper()

	var signingString strings.Builder
	signingString.WriteString(keyID + "\n")
	for _, header := range signedHeaders {
		signingString.WriteString(header + ": " + values[header] + "\n")
	}

	mac := hmac.New(testHashForAlgorithm(t, algorithm), []byte(secret))
	mac.Write([]byte(signingString.String()))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf(
		`Signature keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		keyID,
		algorithm,
		strings.Join(signedHeaders, " "),
		signature,
	)
}

func testHashForAlgorithm(t *testing.T, algorithm string) func() hash.Hash {
	t.Helper()

	switch algorithm {
	case "hmac-sha256":
		return sha256.New
	default:
		t.Fatalf("unsupported test algorithm %q", algorithm)
		return nil
	}
}

func bodyDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
}
