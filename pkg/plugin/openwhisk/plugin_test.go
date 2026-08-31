package openwhisk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPostInitWarnsOnlyForInsecureAPIHost(t *testing.T) {
	tests := []struct {
		name     string
		apiHost  string
		wantWarn bool
	}{
		{name: "http", apiHost: "http://127.0.0.1:3233", wantWarn: true},
		{name: "https", apiHost: "https://127.0.0.1:3233"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var warnings []logger.Entry
			stop := logger.ReplaceObserver("openwhisk-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "openwhisk api_host") {
					warnings = append(warnings, entry)
				}
			})
			defer stop()

			_ = newTestPlugin(t, Config{
				APIHost:      test.apiHost,
				ServiceToken: "test:test",
				Namespace:    "test",
				Action:       "test",
			})
			if got := len(warnings); got != 0 && !test.wantWarn {
				t.Fatalf("warnings = %#v, want none for TLS API host", warnings)
			}
			if got := len(warnings); got != 1 && test.wantWarn {
				t.Fatalf("warnings = %#v, want one insecure API host warning", warnings)
			}
		})
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newOpenWhiskScopedSecretHarness(
		t, 1, "test-route", cfg.ServiceToken, cfg.ServiceToken,
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerInvokesOpenWhiskActionAndUsesJSONResult(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotHost, gotAuthorization, gotContentType, gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHost = r.Host
		gotAuthorization = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read action request body: %v", err)
		}
		gotBody = string(body)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":202,"headers":{"X-Action":"done"},"body":"action body"}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Package:      "samples",
		Action:       "hello",
	})

	res := performRequest(p, "payload")

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "action body" {
		t.Fatalf("response body = %q, want action body", got)
	}
	if got := res.Header().Get("X-Action"); got != "done" {
		t.Fatalf("X-Action = %q, want done", got)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("action method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/namespaces/guest/actions/samples/hello" {
		t.Fatalf("action path = %q, want OpenWhisk action endpoint", gotPath)
	}
	if gotQuery != "blocking=true&result=true&timeout=3000" {
		t.Fatalf("action query = %q, want APISIX 3.17 encoded query order", gotQuery)
	}
	if want := strings.TrimPrefix(api.URL, "http://"); gotHost != want {
		t.Fatalf("action Host = %q, want API authority %q", gotHost, want)
	}
	if gotAuthorization != "Basic dXNlcjpwYXNz" {
		t.Fatalf("Authorization = %q, want Basic dXNlcjpwYXNz", gotAuthorization)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody != "payload" {
		t.Fatalf("action body = %q, want payload", gotBody)
	}
}

func TestRunRequestPhasePublishesUpstreamSource(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":204}`))
	}))
	defer api.Close()
	p := newTestPlugin(t, Config{APIHost: api.URL, ServiceToken: "token", Namespace: "guest", Action: "hello"})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/openwhisk", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("result = %+v, want upstream stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("source = %q, want upstream", lifecycle.ResponseSource())
	}
}

func TestHandlerReturnsServiceUnavailableForInvalidOpenWhiskJSON(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerRelaysScalarAndListResultHeaders(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"headers":{"X-Rate-Limit":7,"X-Values":["one","two"]},"body":{"ok":true}}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})
	res := performRequest(p, "")

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Header().Get("X-Rate-Limit"); got != "7" {
		t.Fatalf("X-Rate-Limit = %q, want 7", got)
	}
	if got := res.Header().Values("X-Values"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("X-Values = %#v, want [one two]", got)
	}
	if got := res.Body.String(); got != `{"ok":true}` {
		t.Fatalf("response body = %q, want JSON object", got)
	}
}

func TestSchemaRejectsInvalidOpenWhiskNames(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"api_host":      "https://openwhisk.example",
		"service_token": "user:pass",
		"namespace":     "bad/name",
		"action":        "hello",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Validate() error = nil, want invalid namespace rejected")
	}
}

func TestSchemaMatchesAPISIX317SanityMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name: "minimal valid configuration",
			config: map[string]any{
				"api_host": "http://127.0.0.1:3233", "service_token": "test:test",
				"namespace": "test", "action": "test",
			},
		},
		{
			name: "missing api_host",
			config: map[string]any{
				"service_token": "test:test", "namespace": "test", "action": "test",
			},
			wantErr: true,
		},
		{
			name: "numeric api_host",
			config: map[string]any{
				"api_host": 3233, "service_token": "test:test", "namespace": "test", "action": "test",
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want APISIX 3.17 schema rejection")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want APISIX 3.17 schema acceptance", err)
			}
		})
	}
}

func TestWriteActionResponseDropsConnectionHeadersForHTTP2(t *testing.T) {
	p := &Plugin{}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			`{"statusCode":200,"headers":{"Connection":"keep-alive","Keep-Alive":"timeout=5","Proxy-Connection":"keep-alive","Upgrade":"websocket","Transfer-Encoding":"chunked","X-Result":"ok"},"body":"done"}`,
		)),
	}
	recorder := httptest.NewRecorder()

	p.writeActionResponse(recorder, response, true)

	for _, field := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade", "Transfer-Encoding"} {
		if got := recorder.Header().Get(field); got != "" {
			t.Fatalf("%s = %q, want removed", field, got)
		}
	}
	if got := recorder.Header().Get("X-Result"); got != "ok" {
		t.Fatalf("X-Result = %q, want ok", got)
	}
	if got := recorder.Body.String(); got != "done" {
		t.Fatalf("body = %q, want done", got)
	}
}

func TestWriteActionResponseFallsBackToOriginalJSONForFalseBody(t *testing.T) {
	original := `{"statusCode":202,"headers":{"X-Action":"done"},"body":false}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(original)),
	}
	recorder := httptest.NewRecorder()

	(&Plugin{}).writeActionResponse(recorder, response, false)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Header().Get("X-Action"); got != "done" {
		t.Fatalf("X-Action = %q, want done", got)
	}
	if got := recorder.Body.String(); got != original {
		t.Fatalf("body = %q, want original JSON %q", got, original)
	}
}

func TestHandlerHonorsDisabledSSLVerify(t *testing.T) {
	api := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":201,"body":"tls ok"}`))
	}))
	defer api.Close()

	sslVerify := false
	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		SSLVerify:    &sslVerify,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d, body=%q", res.Code, http.StatusCreated, res.Body.String())
	}
	if got := res.Body.String(); got != "tls ok" {
		t.Fatalf("response body = %q, want tls ok", got)
	}
}

func TestHandlerRejectsSelfSignedAPIWhenSSLVerifyDefaultsTrue(t *testing.T) {
	api := newQuietTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":204}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")

	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestHandlerRejectsNonTerminalActionStatus(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":600,"body":"should not write"}`))
	}))
	defer api.Close()

	p := newTestPlugin(t, Config{
		APIHost:      api.URL,
		ServiceToken: "user:pass",
		Namespace:    "guest",
		Action:       "hello",
	})

	res := performRequest(p, "")
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func TestPostInitAppliesKeepaliveTransportOptions(t *testing.T) {
	sslVerify := false
	keepalive := false
	p := newTestPlugin(t, Config{
		APIHost:          "https://openwhisk.example",
		SSLVerify:        &sslVerify,
		ServiceToken:     "user:pass",
		Namespace:        "guest",
		Action:           "hello",
		Keepalive:        &keepalive,
		KeepaliveTimeout: 1500,
		KeepalivePool:    7,
	})

	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", p.client.Transport)
	}
	if !transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
	if transport.IdleConnTimeout != 1500*time.Millisecond {
		t.Fatalf("IdleConnTimeout = %s, want 1500ms", transport.IdleConnTimeout)
	}
	if transport.MaxIdleConnsPerHost != 7 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 7", transport.MaxIdleConnsPerHost)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLSClientConfig.InsecureSkipVerify should be true when ssl_verify=false")
	}
}

func TestMaterializeScopedSecretsOwnsOpenWhiskServiceToken(t *testing.T) {
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext("cipher-user:cipher-pass", "openwhisk.service_token")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "literal", raw: "literal-user:literal-pass", resolved: "literal-user:literal-pass"},
		{name: "environment", raw: "$ENV://OPENWHISK_SERVICE_TOKEN", resolved: "env-user:env-pass"},
		{name: "managed", raw: "$secret://vault/openwhisk/token", resolved: "managed-user:managed-pass"},
		{name: "contextual ciphertext", raw: contextual, resolved: "cipher-user:cipher-pass"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secrets, scope, broker, closeAttempt := newOpenWhiskScopedSecretHarness(
				t, uint64(index+1), "openwhisk-materialize", test.raw, test.resolved,
				"0123456789abcdef",
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				APIHost:      "http://openwhisk.invalid",
				ServiceToken: test.raw,
				Namespace:    "guest",
				Action:       "hello",
			}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			calls := broker.scopedCalls()
			isReference := strings.HasPrefix(test.raw, "$secret://") ||
				strings.HasPrefix(strings.ToUpper(test.raw), "$ENV://")
			if !isReference && len(calls) != 0 {
				t.Fatalf("scoped calls = %#v, want none for literal or ciphertext", calls)
			}
			if isReference {
				if len(calls) != 1 {
					t.Fatalf("scoped calls = %#v, want one exact service_token reference", calls)
				}
				call := calls[0]
				if call.Raw != test.raw || call.Scope.Generation != scope.Generation ||
					call.Scope.Domain != generation.DomainHTTP ||
					call.Scope.Plugin != name || call.Scope.Resource != scope.Resource ||
					call.Scope.Source != capability.SecretPluginConfig || call.Scope.Field != "service_token" {
					t.Fatalf("scoped call = %#v, want exact openwhisk.service_token authority", call)
				}
			}
			digest := sha256.Sum256([]byte(test.resolved))
			wantDescriptor := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
			if p.config.ServiceToken != wantDescriptor {
				t.Fatalf(
					"service_token = %q, want exact resolved-plaintext descriptor %q",
					p.config.ServiceToken,
					wantDescriptor,
				)
			}
			if p.client != nil {
				t.Fatal("scoped materialization constructed an HTTP client before PostInit")
			}
		})
	}

	for index, resolved := range []string{"", " \t\n"} {
		t.Run(fmt.Sprintf("reject resolved whitespace %d", index), func(t *testing.T) {
			const raw = "$ENV://OPENWHISK_EMPTY_RETRY"
			secrets, scope, broker, closeAttempt := newOpenWhiskScopedSecretHarness(
				t, uint64(20+index), "openwhisk-empty-retry", raw, resolved,
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				APIHost:      "http://openwhisk.invalid",
				ServiceToken: raw,
				Namespace:    "guest",
				Action:       "hello",
			}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			)
			if err == nil {
				t.Fatal("blank resolved service_token materialized successfully")
			}
			if err.Error() != "materialize plugin secrets: credential unavailable" {
				t.Fatalf("blank materialization error = %q, want constant redaction", err)
			}
			if p.config.ServiceToken != raw || p.serviceTokenSet ||
				p.serviceToken != (secret.Value{}) || p.client != nil {
				t.Fatalf(
					"failed materialization retained state: config=%q scoped=%v value=%#v client=%p",
					p.config.ServiceToken, p.serviceTokenSet, p.serviceToken, p.client,
				)
			}
			broker.setValue(raw, "retry-user:retry-pass")
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			); err != nil {
				t.Fatalf("same-instance retry error = %v", err)
			}
			calls := broker.scopedCalls()
			if len(calls) != 2 {
				t.Fatalf("scoped calls = %#v, want failed attempt plus retry", calls)
			}
			for _, call := range calls {
				if call.Raw != raw || call.Scope.Generation != scope.Generation ||
					call.Scope.Domain != generation.DomainHTTP ||
					call.Scope.Plugin != name || call.Scope.Resource != scope.Resource ||
					call.Scope.Source != capability.SecretPluginConfig || call.Scope.Field != "service_token" {
					t.Fatalf("scoped call = %#v, want exact openwhisk.service_token authority", call)
				}
			}
			digest := sha256.Sum256([]byte("retry-user:retry-pass"))
			wantDescriptor := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
			if p.config.ServiceToken != wantDescriptor {
				t.Fatalf("retried service_token = %q, want %q", p.config.ServiceToken, wantDescriptor)
			}
		})
	}
}

func TestOpenWhiskScopedMaterializationFailureIsAtomicRedactedAndRetryable(t *testing.T) {
	const raw = "$secret://vault/openwhisk/failure"
	secrets, scope, broker, closeAttempt := newOpenWhiskScopedSecretHarness(
		t, 22, "openwhisk-failure", raw, "private-user:private-pass",
	)
	defer closeAttempt()
	broker.setFailure(raw)
	p := &Plugin{config: Config{
		APIHost:      "http://openwhisk.invalid",
		ServiceToken: raw,
		Namespace:    "guest",
		Action:       "hello",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-user") ||
		strings.Contains(err.Error(), "private-openwhisk-token") {
		t.Fatalf("materialization error leaked secret details: %v", err)
	}
	if p.config.ServiceToken != raw || p.serviceTokenSet || p.serviceToken != (secret.Value{}) || p.client != nil {
		t.Fatalf(
			"failed materialization retained state: config=%q scoped=%v value=%#v client=%p",
			p.config.ServiceToken, p.serviceTokenSet, p.serviceToken, p.client,
		)
	}
	broker.setFailure("")
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	digest := sha256.Sum256([]byte("private-user:private-pass"))
	wantDescriptor := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if p.config.ServiceToken != wantDescriptor || !p.serviceTokenSet {
		t.Fatalf("retry state = %q/%v, want installed descriptor", p.config.ServiceToken, p.serviceTokenSet)
	}
}

func TestOpenWhiskScopedMaterializationRejectsWrongAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*secret.Scope)
	}{
		{name: "wrong factory", mutate: func(scope *secret.Scope) { scope.Plugin = "openfunction" }},
		{name: "wrong source", mutate: func(scope *secret.Scope) {
			scope.Source = capability.SecretPluginMetadata
		}},
		{name: "outside closure", mutate: func(scope *secret.Scope) {
			scope.Resource.ID = "another-route"
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const raw = "$ENV://OPENWHISK_AUTHORITY"
			secrets, scope, broker, closeAttempt := newOpenWhiskScopedSecretHarness(
				t, uint64(50+index), "openwhisk-authority", raw, "authority-user:authority-pass",
			)
			defer closeAttempt()
			test.mutate(&scope)
			p := &Plugin{config: Config{ServiceToken: raw}}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			); err == nil {
				t.Fatal("wrong scoped authority materialized successfully")
			}
			if got := broker.scopedCalls(); len(got) != 0 {
				t.Fatalf("wrong authority reached resolver: %#v", got)
			}
			if p.config.ServiceToken != raw || p.serviceTokenSet {
				t.Fatal("wrong authority changed plugin secret state")
			}
		})
	}
}

func TestOpenWhiskScopedMaterializationUsesResolvedPlaintextDescriptor(t *testing.T) {
	const raw = "$ENV://OPENWHISK_LEGACY_SERVICE_TOKEN"
	const resolved = "legacy-user:legacy-pass"
	p := &Plugin{config: Config{
		APIHost:      "http://openwhisk.invalid",
		ServiceToken: raw,
		Namespace:    "guest",
		Action:       "hello",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, cleanup := newOpenWhiskScopedSecretHarness(
		t, 1, "scoped-descriptor", raw, resolved,
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	digest := sha256.Sum256([]byte("legacy-user:legacy-pass"))
	wantDescriptor := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if p.config.ServiceToken != wantDescriptor {
		t.Fatalf("legacy service_token = %q, want %q", p.config.ServiceToken, wantDescriptor)
	}
	if p.client != nil {
		t.Fatal("legacy materialization constructed an HTTP client before PostInit")
	}
}

func TestOpenWhiskPostInitRejectsUnmaterializedServiceTokenWithoutClient(t *testing.T) {
	p := &Plugin{config: Config{ServiceToken: "$ENV://OPENWHISK_UNPREPARED"}}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want credential unavailable", err)
	}
	if p.client != nil {
		t.Fatal("PostInit constructed a client without a materialized service_token")
	}
}

func TestOpenWhiskFailedResponseRetainsNoDerivedAuthorization(t *testing.T) {
	const token = "retained-user:retained-pass"
	p := newTestPlugin(t, Config{
		APIHost:      "http://openwhisk.invalid",
		ServiceToken: token,
		Namespace:    "guest",
		Action:       "hello",
	})
	capture := &retainingRoundTripper{}
	client := &http.Client{Transport: capture}
	p.client = client

	response := performRequest(p, "request-body")
	p.Stop()
	assertOpenWhiskCredentialAbsentFromRetainedGraph(
		t, token, "request-body", p, client, capture, response,
	)
}

func TestOpenWhiskScopedFailedResponseRetainsNoDerivedAuthorization(t *testing.T) {
	const (
		raw   = "$ENV://OPENWHISK_SCOPED_RETENTION"
		token = "scoped-user:scoped-pass"
	)
	secrets, scope, _, closeAttempt := newOpenWhiskScopedSecretHarness(
		t, 21, "openwhisk-scoped-retention", raw, token,
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		APIHost:      "http://openwhisk.invalid",
		ServiceToken: raw,
		Namespace:    "guest",
		Action:       "hello",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	capture := &retainingRoundTripper{}
	client := &http.Client{Transport: capture}
	p.client = client

	response := performRequest(p, "scoped-body")
	p.Stop()
	assertOpenWhiskCredentialAbsentFromRetainedGraph(
		t, token, "scoped-body", p, client, capture, response,
	)
}

func assertOpenWhiskCredentialAbsentFromRetainedGraph(
	t *testing.T,
	token string,
	wantBody string,
	p *Plugin,
	client *http.Client,
	capture *retainingRoundTripper,
	response *httptest.ResponseRecorder,
) {
	t.Helper()
	wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed response status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if capture.request == nil || capture.response == nil || capture.response.Request == nil || capture.body == nil {
		t.Fatal("retaining transport did not observe complete request/response/body objects")
	}
	if !capture.body.closed.Load() {
		t.Fatal("failed third-party response body was not closed")
	}
	for object, request := range map[string]*http.Request{
		"transport request": capture.request,
		"response request":  capture.response.Request,
	} {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Fatalf("%s retained Authorization %q after callback and Stop", object, got)
		}
		if request.GetBody == nil {
			t.Fatalf("%s GetBody = nil, want replayable request body", object)
		}
		body, err := request.GetBody()
		if err != nil {
			t.Fatalf("%s GetBody() error = %v", object, err)
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatalf("read %s GetBody(): %v", object, err)
		}
		if got := string(data); got != wantBody {
			t.Fatalf("%s replay body = %q, want %q", object, got, wantBody)
		}
		if string(data) == token || string(data) == wantAuthorization {
			t.Fatalf("%s GetBody retained exact service token or Basic authorization", object)
		}
	}

	objects := map[string]any{
		"plugin":             p,
		"saved client":       client,
		"saved transport":    capture,
		"transport request":  capture.request,
		"transport response": capture.response,
		"response request":   capture.response.Request,
		"response body":      capture.body,
		"handler response":   response,
	}
	for label, object := range objects {
		for _, forbidden := range []string{token, wantAuthorization} {
			if objectGraphContainsExactString(
				reflect.ValueOf(object), forbidden, make(map[uintptr]struct{}), 0,
			) {
				t.Fatalf("%s retained exact credential %q after callback and Stop", label, forbidden)
			}
		}
	}
}

func objectGraphContainsExactString(
	value reflect.Value,
	want string,
	visited map[uintptr]struct{},
	depth int,
) bool {
	if !value.IsValid() || depth > 32 {
		return false
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String() == want
	case reflect.Pointer:
		if value.IsNil() {
			return false
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return false
		}
		visited[pointer] = struct{}{}
		return objectGraphContainsExactString(value.Elem(), want, visited, depth+1)
	case reflect.Struct:
		for _, field := range value.Fields() {
			if objectGraphContainsExactString(field, want, visited, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 && value.Len() == len(want) {
			matched := true
			for index := range value.Len() {
				if byte(value.Index(index).Uint()) != want[index] {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
		for index := range value.Len() {
			if objectGraphContainsExactString(value.Index(index), want, visited, depth+1) {
				return true
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if objectGraphContainsExactString(iterator.Key(), want, visited, depth+1) ||
				objectGraphContainsExactString(iterator.Value(), want, visited, depth+1) {
				return true
			}
		}
	}
	return false
}

func TestOpenWhiskGenerationsDoNotShareAuthorizationOrRetirement(t *testing.T) {
	var authorizationsMu sync.Mutex
	var authorizations []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationsMu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		authorizationsMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":200,"body":"ok"}`))
	}))
	defer api.Close()

	pN, closeN := newScopedOpenWhiskPlugin(
		t, 31, "openwhisk-n", api.URL, "$ENV://OPENWHISK_N", "user-n:pass-n",
	)
	defer closeN()
	pN1, closeN1 := newScopedOpenWhiskPlugin(
		t, 32, "openwhisk-n1", api.URL, "$ENV://OPENWHISK_N1", "user-n1:pass-n1",
	)
	defer closeN1()

	if response := performRequest(pN, "n"); response.Code != http.StatusOK {
		t.Fatalf("generation N status = %d, want 200", response.Code)
	}
	if response := performRequest(pN1, "n1"); response.Code != http.StatusOK {
		t.Fatalf("generation N+1 status = %d, want 200", response.Code)
	}
	pN.Stop()
	pN.Stop()
	if response := performRequest(pN, "retired"); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("retired generation status = %d, want 503", response.Code)
	}
	if response := performRequest(pN1, "n1-retained"); response.Code != http.StatusOK {
		t.Fatalf("retained generation N+1 status = %d, want 200", response.Code)
	}

	authorizationsMu.Lock()
	defer authorizationsMu.Unlock()
	want := []string{
		"Basic dXNlci1uOnBhc3Mtbg==",
		"Basic dXNlci1uMTpwYXNzLW4x",
		"Basic dXNlci1uMTpwYXNzLW4x",
	}
	if fmt.Sprint(authorizations) != fmt.Sprint(want) {
		t.Fatalf("upstream authorizations = %#v, want %#v", authorizations, want)
	}
}

func TestOpenWhiskScopedRequestsBlockStopUntilUpstreamRetires(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string) (*Plugin, func())
		want    string
	}{
		{
			name: "scoped",
			prepare: func(t *testing.T, apiHost string) (*Plugin, func()) {
				return newScopedOpenWhiskPlugin(
					t, 41, "openwhisk-scoped-barrier", apiHost,
					"$ENV://OPENWHISK_SCOPED_BARRIER", "scoped-user:scoped-pass",
				)
			},
			want: "Basic c2NvcGVkLXVzZXI6c2NvcGVkLXBhc3M=",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamEntered := make(chan string, 1)
			releaseUpstream := make(chan struct{})
			var releaseUpstreamOnce sync.Once
			release := func() { releaseUpstreamOnce.Do(func() { close(releaseUpstream) }) }
			var upstreamCalls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				upstreamEntered <- r.Header.Get("Authorization")
				<-releaseUpstream
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"statusCode":200,"body":"ok"}`))
			}))
			defer func() {
				release()
				api.Close()
			}()
			p, closeAttempt := test.prepare(t, api.URL)
			defer closeAttempt()
			requestDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { requestDone <- performRequest(p, "barrier") }()
			var gotAuthorization string
			select {
			case gotAuthorization = <-upstreamEntered:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for real upstream request")
			}
			if gotAuthorization != test.want {
				t.Fatalf("upstream Authorization = %q, want %q", gotAuthorization, test.want)
			}

			stopDone := make(chan struct{})
			go func() {
				p.Stop()
				close(stopDone)
			}()
			deadline := time.Now().Add(time.Second)
			for p.lifecycleMu.TryRLock() {
				p.lifecycleMu.RUnlock()
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for Stop to wait on the lifecycle write gate")
				}
				time.Sleep(time.Millisecond)
			}
			select {
			case <-stopDone:
				t.Fatal("Stop returned while the upstream request was in flight")
			case <-time.After(100 * time.Millisecond):
			}
			release()
			select {
			case response := <-requestDone:
				if response.Code != http.StatusOK {
					t.Fatalf("in-flight response status = %d, want 200", response.Code)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for in-flight request retirement")
			}
			select {
			case <-stopDone:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for Stop after upstream retirement")
			}

			if response := performRequest(p, "retired"); response.Code != http.StatusServiceUnavailable {
				t.Fatalf("retired response status = %d, want 503", response.Code)
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream calls after retired request = %d, want 1", got)
			}
			if p.client != nil || p.serviceTokenSet ||
				p.serviceToken != (secret.Value{}) || !p.retired {
				t.Fatalf(
					"retired state = client:%p scoped:%v value:%#v retired:%v",
					p.client, p.serviceTokenSet, p.serviceToken, p.retired,
				)
			}
			p.Stop()
		})
	}
}

func TestOpenWhiskHandlerAndStopAreSafeConcurrently(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"statusCode":200,"body":"ok"}`))
	}))
	defer api.Close()
	p, closeAttempt := newScopedOpenWhiskPlugin(
		t, 42, "openwhisk-concurrent", api.URL,
		"$ENV://OPENWHISK_CONCURRENT", "concurrent-user:concurrent-pass",
	)
	defer closeAttempt()

	start := make(chan struct{})
	responses := make(chan int, 32)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			<-start
			responses <- performRequest(p, "concurrent").Code
		})
	}
	group.Go(func() {
		<-start
		p.Stop()
	})
	close(start)
	group.Wait()
	close(responses)
	for status := range responses {
		if status != http.StatusOK && status != http.StatusServiceUnavailable {
			t.Fatalf("concurrent handler status = %d, want 200 or retired 503", status)
		}
	}
}

func TestOpenWhiskAuthorizationDerivationStaysInsidePrivateUseCallback(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "plugin.go", nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(plugin.go) error = %v", err)
	}

	var stack []ast.Node
	var authorizationSets int
	var authorizationEncodes int
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, node)
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isAuthorizationHeaderSet(call) {
			authorizationSets++
			if !insidePrivateTokenCallback(stack) {
				violations = append(violations, fileSet.Position(call.Pos()).String())
			}
		}
		if isAuthorizationEncode(call) {
			authorizationEncodes++
			if !insidePrivateTokenCallback(stack) {
				violations = append(violations, fileSet.Position(call.Pos()).String())
			}
		}
		return true
	})
	if authorizationSets != 1 || authorizationEncodes != 1 || len(violations) != 0 {
		t.Fatalf(
			"Authorization set/encode sites/private-callback violations = %d/%d/%v, want one/one/none",
			authorizationSets, authorizationEncodes, violations,
		)
	}
}

func isAuthorizationHeaderSet(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Set" || len(call.Args) < 2 {
		return false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	return ok && literal.Kind == token.STRING && literal.Value == `"Authorization"`
}

func isAuthorizationEncode(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "EncodeToString"
}

func insidePrivateTokenCallback(stack []ast.Node) bool {
	for index := len(stack) - 1; index >= 1; index-- {
		function, ok := stack[index].(*ast.FuncLit)
		if !ok {
			continue
		}
		parent, ok := stack[index-1].(*ast.CallExpr)
		if !ok {
			return false
		}
		selector, ok := parent.Fun.(*ast.SelectorExpr)
		return ok && selector.Sel.Name == "useServiceTokenLocked" &&
			len(parent.Args) == 1 && parent.Args[0] == function
	}
	return false
}

func newScopedOpenWhiskPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	apiHost string,
	raw string,
	resolved string,
) (*Plugin, func()) {
	t.Helper()
	secrets, scope, _, closeAttempt := newOpenWhiskScopedSecretHarness(
		t, revision, resourceID, raw, resolved,
	)
	p := &Plugin{config: Config{
		APIHost:      apiHost,
		ServiceToken: raw,
		Namespace:    "guest",
		Action:       "hello",
	}}
	if err := p.Init(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	return p, closeAttempt
}

func performRequest(p *Plugin, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPatch, "http://example.com/hello?client=ignored", strings.NewReader(body))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)
	return rr
}

func newQuietTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(testLogWriter{}, "", 0)
	server.StartTLS()
	return server
}

type testLogWriter struct{}

func (testLogWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

type retainingRoundTripper struct {
	request  *http.Request
	response *http.Response
	body     *trackingReadCloser
}

func (transport *retainingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	transport.body = &trackingReadCloser{Reader: strings.NewReader("not-json")}
	transport.response = &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     make(http.Header),
		Body:       transport.body,
		Request:    request,
	}
	return transport.response, nil
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (body *trackingReadCloser) Close() error {
	body.closed.Store(true)
	return nil
}

type openWhiskScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type openWhiskScopedSecretBroker struct {
	mu      sync.Mutex
	values  map[string]string
	failRaw string
	calls   []openWhiskScopedSecretCall
}

func (broker *openWhiskScopedSecretBroker) ResolveScoped(
	_ context.Context, scope secret.Scope, raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, openWhiskScopedSecretCall{Scope: scope, Raw: raw})
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver failed for %s private-openwhisk-token", raw)
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (broker *openWhiskScopedSecretBroker) scopedCalls() []openWhiskScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]openWhiskScopedSecretCall(nil), broker.calls...)
}

func (broker *openWhiskScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func (broker *openWhiskScopedSecretBroker) setFailure(raw string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.failRaw = raw
}

func newOpenWhiskScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	raw string,
	resolved string,
	keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *openWhiskScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID,
		"plugins": map[string]any{name: map[string]any{
			"api_host":      "http://openwhisk.invalid",
			"service_token": raw,
			"namespace":     "guest",
			"action":        "hello",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{Key: key, Value: document}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "openwhisk-test",
		}},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &openWhiskScopedSecretBroker{values: map[string]string{raw: resolved}}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}
