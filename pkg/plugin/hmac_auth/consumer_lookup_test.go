package hmac_auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func newHMACConsumerLookup(
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

func hmacConsumerRecord(id, username, keyID, secretKey string) runtime.ConsumerRecord {
	plugins := map[string]resource.PluginConfig{}
	if keyID != "" || secretKey != "" {
		plugins[name] = map[string]any{"key_id": keyID, "secret_key": secretKey}
	}
	return runtime.ConsumerRecord{
		ID: id,
		Consumer: resource.Consumer{
			Username: username,
			Plugins:  plugins,
		},
	}
}

func bindHMACConsumer(id, keyID string) runtime.ConsumerCredentialBinding {
	return runtime.ConsumerCredentialBinding{Plugin: name, Key: keyID, ConsumerID: id}
}

func performHMACLookupRequest(
	t *testing.T,
	p *Plugin,
	authorization string,
	date string,
	digest string,
	body string,
	next http.Handler,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://example.com/get", strings.NewReader(body))
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Date", date)
	if digest != "" {
		request.Header.Set("Digest", digest)
	}
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(response, request)
	return response
}

func TestHMACConsumerLookupUsesResolvedSecretForCanonicalBodyValidation(t *testing.T) {
	const keyID = "hmac-lookup-resolved-key"
	addHMACConsumer(t, "hmac-store-poison-resolved", keyID, "store-poison-secret")
	lookup := newHMACConsumerLookup(t,
		[]runtime.ConsumerRecord{hmacConsumerRecord(
			"hmac-resolved", "hmac-resolved-user", keyID, "resolved-hmac-secret",
		)},
		[]runtime.ConsumerCredentialBinding{bindHMACConsumer("hmac-resolved", keyID)},
	)
	p := newTestPlugin(t, Config{
		ValidateRequestBody: true,
		SignedHeaders:       []string{"date", "digest"},
	})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	date := time.Now().UTC().Format(http.TimeFormat)
	body := "attempt-owned-body"
	digest := bodyDigest(body)
	authorization := signatureHeader(
		t, keyID, "resolved-hmac-secret", "hmac-sha256", []string{"date", "digest"},
		map[string]string{"date": date, "digest": digest},
	)

	response := performHMACLookupRequest(
		t, p, authorization, date, digest, body,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-resolved-user" {
				t.Fatalf("consumer_name = %v, want hmac-resolved-user", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestHMACConsumerLookupMissDoesNotFallBackToStore(t *testing.T) {
	const keyID = "hmac-authoritative-miss-key"
	addHMACConsumer(t, "hmac-store-poison-miss", keyID, "store-only-secret")
	lookup := newHMACConsumerLookup(t, nil, nil)
	p := newTestPlugin(t, Config{})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	date := time.Now().UTC().Format(http.TimeFormat)
	authorization := signatureHeader(
		t, keyID, "store-only-secret", "hmac-sha256", []string{"date"},
		map[string]string{"date": date},
	)

	response := performHMACLookupRequest(
		t, p, authorization, date, "", "",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("lookup miss reached Store-backed consumer")
		}),
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestHMACAnonymousConsumerUsesLookupByID(t *testing.T) {
	const anonymousID = "hmac-lookup-anonymous"
	lookup := newHMACConsumerLookup(t,
		[]runtime.ConsumerRecord{hmacConsumerRecord(anonymousID, "hmac-lookup-anonymous-user", "", "")},
		nil,
	)
	p := newTestPlugin(t, Config{AnonymousConsumer: anonymousID})
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-lookup-anonymous-user" {
			t.Fatalf("consumer_name = %v, want hmac-lookup-anonymous-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestHMACAnonymousLookupMissDoesNotFallBackToStore(t *testing.T) {
	const anonymousID = "hmac-store-only-anonymous"
	addConsumer(t, anonymousID)
	lookup := newHMACConsumerLookup(t, nil, nil)
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

func TestHMACConsumerLookupsAreAttemptIsolatedAndClosingNPreservesNPlusOne(t *testing.T) {
	const keyID = "hmac-overlap-key"
	addHMACConsumer(t, "hmac-overlap-store-poison", keyID, "store-poison-secret")
	firstLookup := newHMACConsumerLookup(t,
		[]runtime.ConsumerRecord{hmacConsumerRecord("hmac-n", "hmac-user-n", keyID, "hmac-secret-n")},
		[]runtime.ConsumerCredentialBinding{bindHMACConsumer("hmac-n", keyID)},
	)
	secondLookup := newHMACConsumerLookup(t,
		[]runtime.ConsumerRecord{hmacConsumerRecord(
			"hmac-n-plus-one", "hmac-user-n-plus-one", keyID, "hmac-secret-n-plus-one",
		)},
		[]runtime.ConsumerCredentialBinding{bindHMACConsumer("hmac-n-plus-one", keyID)},
	)
	first := newTestPlugin(t, Config{})
	first.SetDependencies(base.Dependencies{Consumers: firstLookup})
	second := newTestPlugin(t, Config{})
	second.SetDependencies(base.Dependencies{Consumers: secondLookup})
	date := time.Now().UTC().Format(http.TimeFormat)
	firstAuth := signatureHeader(
		t,
		keyID,
		"hmac-secret-n",
		"hmac-sha256",
		[]string{"date"},
		map[string]string{"date": date},
	)
	secondAuth := signatureHeader(
		t,
		keyID,
		"hmac-secret-n-plus-one",
		"hmac-sha256",
		[]string{"date"},
		map[string]string{"date": date},
	)

	if response := performHMACLookupRequest(
		t, first, firstAuth, date, "", "",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-user-n" {
				t.Fatalf("generation N consumer_name = %v", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N response code = %d; body=%s", response.Code, response.Body.String())
	}
	if response := performHMACLookupRequest(
		t, second, secondAuth, date, "", "",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-user-n-plus-one" {
				t.Fatalf("generation N+1 consumer_name = %v", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N+1 response code = %d; body=%s", response.Code, response.Body.String())
	}

	firstLookup.Close()
	if response := performHMACLookupRequest(
		t, first, firstAuth, date, "", "",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("closed generation N reached downstream")
		}),
	); response.Code != http.StatusUnauthorized {
		t.Fatalf("closed generation N response code = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if response := performHMACLookupRequest(
		t, second, secondAuth, date, "", "",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "hmac-user-n-plus-one" {
				t.Fatalf("generation N+1 after close consumer_name = %v", got)
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	); response.Code != http.StatusNoContent {
		t.Fatalf("generation N+1 after closing N response code = %d; body=%s", response.Code, response.Body.String())
	}
}
