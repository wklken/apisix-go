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
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

type hmacTestFixture struct {
	sync.Mutex
	consumers   []runtime.ConsumerRecord
	credentials []runtime.ConsumerCredentialBinding
}

var hmacTestFixtures sync.Map

func hmacFixtureFor(t *testing.T) *hmacTestFixture {
	t.Helper()
	fixture := &hmacTestFixture{}
	actual, loaded := hmacTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { hmacTestFixtures.Delete(t) })
	}
	return actual.(*hmacTestFixture)
}

func addHMACConsumer(t *testing.T, username, keyID, secretKey string) {
	t.Helper()
	fixture := hmacFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(fixture.consumers, runtime.ConsumerRecord{
		ID: username,
		Consumer: resource.Consumer{Username: username, Plugins: map[string]resource.PluginConfig{
			name: map[string]any{"key_id": keyID, "secret_key": secretKey},
		}},
	})
	fixture.credentials = append(fixture.credentials, runtime.ConsumerCredentialBinding{
		Plugin: name, Key: keyID, ConsumerID: username,
	})
}

func addConsumer(t *testing.T, username string) {
	t.Helper()
	fixture := hmacFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(fixture.consumers, runtime.ConsumerRecord{
		ID: username, Consumer: resource.Consumer{Username: username, Plugins: map[string]resource.PluginConfig{}},
	})
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	fixture := hmacFixtureFor(t)
	fixture.Lock()
	consumers := append([]runtime.ConsumerRecord(nil), fixture.consumers...)
	credentials := append([]runtime.ConsumerCredentialBinding(nil), fixture.credentials...)
	fixture.Unlock()
	lookup, err := runtime.NewConsumerBindings(consumers, nil, credentials)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	t.Cleanup(lookup.Close)

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

func TestHandlerRecordsProbeAndMissingAnonymousDiagnostics(t *testing.T) {
	p := newTestPlugin(t, Config{AnonymousConsumer: "missing-probe-anonymous"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing HMAC authorization reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 2 || !strings.Contains(diagnostics[0], "missing Authorization header") ||
		!strings.Contains(diagnostics[1], "failed to get anonymous consumer missing-probe-anonymous") {
		t.Fatalf("probe diagnostics = %v, want ordered auth and anonymous failures", diagnostics)
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
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	response := httptest.NewRecorder()
	nextCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "consumer-plugin-hmac-user" {
			t.Fatalf("consumer_name = %v, want consumer-plugin-hmac-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, req)

	if nextCalls != 1 {
		t.Fatalf("next handler calls = %d, want 1", nextCalls)
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

func TestPostInitCapsHMACBodyAtIngressLimit(t *testing.T) {
	p := &Plugin{config: Config{ValidateRequestBody: true, MaxReqBodySize: 64 * 1024 * 1024}}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{Config: config.Config{
		NginxConfig: config.NginxConfig{HTTP: config.NginxHTTP{ClientMaxBodySize: 1024}},
	}}})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if enabled, limit := p.config.BodyIsolation(); !enabled || limit != 1024 {
		t.Fatalf("BodyIsolation() = %t/%d, want ingress cap 1024", enabled, limit)
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

func TestHandlerRejectsSignedHeaderWithoutRequestValue(t *testing.T) {
	addHMACConsumer(t, "missing-header-user", "missing-header-key", "missing-header-secret")
	p := newTestPlugin(t, Config{
		SignedHeaders: []string{"date", "x-tenant"},
		ClockSkew:     1_000_000_000,
	})
	date := "Thu, 24 Sep 2020 06:39:52 GMT"
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	params := signatureParams{
		KeyID:     "missing-header-key",
		Algorithm: "hmac-sha256",
		Headers:   []string{"date", "x-tenant"},
	}
	generated, err := generateSignature(req, "missing-header-secret", params)
	if err != nil {
		t.Fatalf("generateSignature() error = %v", err)
	}
	auth := fmt.Sprintf(
		`Signature keyId="%s",algorithm="%s",headers="%s",signature="%s"`,
		params.KeyID,
		params.Algorithm,
		strings.Join(params.Headers, " "),
		base64.StdEncoding.EncodeToString(generated),
	)
	req.Header.Set("Authorization", auth)
	recorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing signed header reached downstream")
	})).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
}

func TestHandlerAcceptsRequiredHostFromRequestAuthority(t *testing.T) {
	addHMACConsumer(t, "host-header-user", "host-header-key", "host-header-secret")
	p := newTestPlugin(t, Config{
		SignedHeaders: []string{"date", "host"},
		ClockSkew:     1_000_000_000,
	})
	date := "Thu, 24 Sep 2020 06:39:52 GMT"
	auth := signatureHeader(
		t,
		"host-header-key",
		"host-header-secret",
		"hmac-sha256",
		[]string{"date", "host"},
		map[string]string{"date": date, "host": "example.com"},
	)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", auth)
	recorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
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
