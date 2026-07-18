package multi_auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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
		s := store.NewStore(t.TempDir()+"/multi-auth.db", testEvents)
		s.Start()
	})
}

func addAuthConsumer(t *testing.T, username string, plugins map[string]any) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins":  plugins,
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
}

func waitForConsumerKey(t *testing.T, pluginName string, key string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey(pluginName, key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer key %s:%s was not indexed", pluginName, key)
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

func TestHandlerAllowsRequestWhenAnyAuthPluginSucceeds(t *testing.T) {
	addAuthConsumer(t, "key-user", map[string]any{
		"key-auth": map[string]any{"key": "valid-key"},
	})
	waitForConsumerKey(t, "key-auth", "valid-key")

	hideCredentials := true
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"basic-auth": {}},
			{"key-auth": {"hide_credentials": hideCredentials, "header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "valid-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "key-user" {
			t.Fatalf("consumer_name = %v, want key-user", got)
		}
		if got := r.Header.Get("apikey"); got != "" {
			t.Fatalf("apikey header = %q, want hidden", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerPreservesRejectingConsumerPluginResponse(t *testing.T) {
	addAuthConsumer(t, "rejecting-consumer-user", map[string]any{
		"key-auth": map[string]any{"key": "rejecting-consumer-key"},
	})
	waitForConsumerKey(t, "key-auth", "rejecting-consumer-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {"header": "apikey"}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "rejecting-consumer-key")
	runnerCalls := 0
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, _ http.Handler) {
		runnerCalls++
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "rejecting-consumer-user" {
			t.Fatalf("consumer_name = %v, want rejecting-consumer-user", got)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("consumer rejected"))
	})
	res := httptest.NewRecorder()
	downstreamCalls := 0

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		downstreamCalls++
	})).ServeHTTP(res, req)

	if runnerCalls != 1 {
		t.Fatalf("consumer runner calls = %d, want 1", runnerCalls)
	}
	if downstreamCalls != 0 {
		t.Fatalf("downstream calls = %d, want 0", downstreamCalls)
	}
	if res.Code != http.StatusForbidden || res.Body.String() != "consumer rejected" {
		t.Fatalf("response = %d %q, want 403 consumer rejected", res.Code, res.Body.String())
	}
}

func TestHandlerRunsAcceptingConsumerPluginsAndDownstreamOnce(t *testing.T) {
	addAuthConsumer(t, "accepting-consumer-user", map[string]any{
		"key-auth": map[string]any{"key": "accepting-consumer-key"},
	})
	waitForConsumerKey(t, "key-auth", "accepting-consumer-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {"header": "apikey"}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "accepting-consumer-key")
	runnerCalls := 0
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		runnerCalls++
		next.ServeHTTP(w, r)
	})
	res := httptest.NewRecorder()
	downstreamCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if runnerCalls != 1 {
		t.Fatalf("consumer runner calls = %d, want 1", runnerCalls)
	}
	if downstreamCalls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstreamCalls)
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
}

func TestHandlerPassesConsumerRunnerRequestContextToDownstream(t *testing.T) {
	type runnerContextKey struct{}

	addAuthConsumer(t, "context-consumer-user", map[string]any{
		"key-auth": map[string]any{"key": "context-consumer-key"},
	})
	waitForConsumerKey(t, "key-auth", "context-consumer-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {"header": "apikey"}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "context-consumer-key")
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		r = ctx.WithConsumerPluginOverrides(r, map[string]struct{}{"consumer-restriction": {}})
		r = r.WithContext(context.WithValue(r.Context(), runnerContextKey{}, "from-runner"))
		next.ServeHTTP(w, r)
	})
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ctx.ConsumerPluginOverrides(r, "consumer-restriction") {
			t.Fatal("consumer plugin override did not reach downstream")
		}
		if got := r.Context().Value(runnerContextKey{}); got != "from-runner" {
			t.Fatalf("runner context = %v, want from-runner", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
}

func TestHandlerAllowsBasicAuthWhenLaterPluginWouldFail(t *testing.T) {
	addAuthConsumer(t, "basic-user", map[string]any{
		"basic-auth": map[string]any{"username": "basic-user", "password": "secret"},
	})
	waitForConsumerKey(t, "basic-auth", "basic-user")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("basic-user:secret")))
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "basic-user" {
			t.Fatalf("consumer_name = %v, want basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerDoesNotLetFailedAuthMutateLaterAlternative(t *testing.T) {
	addAuthConsumer(t, "basic-after-jwt-user", map[string]any{
		"basic-auth": map[string]any{"username": "basic-after-jwt-user", "password": "secret"},
	})
	waitForConsumerKey(t, "basic-auth", "basic-after-jwt-user")

	hideCredentials := true
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"jwt-auth": {"hide_credentials": hideCredentials}},
			{"basic-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("basic-after-jwt-user:secret")))
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "basic-after-jwt-user" {
			t.Fatalf("consumer_name = %v, want basic-after-jwt-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerRestoresBodyAfterFailedHMACAlternative(t *testing.T) {
	addAuthConsumer(t, "body-fallback-user", map[string]any{
		"hmac-auth": map[string]any{"key_id": "body-hmac-key", "secret_key": "body-hmac-secret"},
		"key-auth":  map[string]any{"key": "body-api-key"},
	})
	waitForConsumerKey(t, "hmac-auth", "body-hmac-key")
	waitForConsumerKey(t, "key-auth", "body-api-key")

	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"hmac-auth": {"validate_request_body": true, "max_req_body_size": 10}},
		{"key-auth": {"header": "apikey"}},
	}})
	body := "body that is longer than ten bytes"
	req := httptest.NewRequest(http.MethodPost, "/body", strings.NewReader(body))
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithRequestVars(req)
	req.Header.Set("apikey", "body-api-key")
	req.Header.Set("Digest", "SHA-256=unused")
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	signing := "body-hmac-key\nPOST /body\ndate: " + date + "\n"
	mac := hmac.New(sha256.New, []byte("body-hmac-secret"))
	_, _ = mac.Write([]byte(signing))
	req.Header.Set(
		"Authorization",
		`Signature keyId="body-hmac-key",algorithm="hmac-sha256",headers="@request-target date",signature="`+
			base64.StdEncoding.EncodeToString(mac.Sum(nil))+`"`,
	)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read downstream body: %v", err)
		}
		if string(got) != body {
			t.Fatalf("downstream body = %q, want %q", got, body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestPostInitRejectsAuthPluginEntryWithMultiplePlugins(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}, "key-auth": {}},
		{"jwt-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "exactly one auth plugin") {
		t.Fatalf("PostInit() error = %v, want exactly-one-plugin diagnostic", err)
	}
}

func TestStatusOnlyAuthFailureDoesNotPanic(t *testing.T) {
	req := newMultiAuthRequest()
	authenticated, failure := (configuredAuth{name: "status-only-auth", plugin: statusOnlyAuth{}}).succeeds(req)
	if authenticated != nil || failure.status != http.StatusUnauthorized || failure.message != "" {
		t.Fatalf(
			"status-only auth result = (%v, %+v), want nil request with 401 empty-message failure",
			authenticated,
			failure,
		)
	}
}

func TestProbeResponseWriterBoundsFailureDiagnostic(t *testing.T) {
	writer := &probeResponseWriter{header: http.Header{}}
	body := make([]byte, maxFailureDiagnosticBytes+1024)
	written, err := writer.Write(body)
	if err != nil || written != len(body) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", written, err, len(body))
	}
	if writer.body.Len() != maxFailureDiagnosticBytes {
		t.Fatalf("captured diagnostic bytes = %d, want %d", writer.body.Len(), maxFailureDiagnosticBytes)
	}
}

type statusOnlyAuth struct{}

func (statusOnlyAuth) Init() error       { return nil }
func (statusOnlyAuth) PostInit() error   { return nil }
func (statusOnlyAuth) Config() any       { return &struct{}{} }
func (statusOnlyAuth) GetSchema() string { return `{}` }
func (statusOnlyAuth) Handler(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func TestHandlerAllowsKeyAuthAfterLDAPAuthMissingCredentials(t *testing.T) {
	addAuthConsumer(t, "ldap-fallback-key-user", map[string]any{
		"key-auth": map[string]any{"key": "ldap-fallback-key"},
	})
	waitForConsumerKey(t, "key-auth", "ldap-fallback-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"ldap-auth": {"base_dn": "dc=example,dc=org", "ldap_uri": "ldap://127.0.0.1:389"}},
			{"key-auth": {"header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "ldap-fallback-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "ldap-fallback-key-user" {
			t.Fatalf("consumer_name = %v, want ldap-fallback-key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerAllowsKeyAuthAfterJWEDecryptMissingToken(t *testing.T) {
	addAuthConsumer(t, "jwe-fallback-key-user", map[string]any{
		"key-auth": map[string]any{"key": "jwe-fallback-key"},
	})
	waitForConsumerKey(t, "key-auth", "jwe-fallback-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"jwe-decrypt": {"header": "Authorization", "forward_header": "Authorization"}},
			{"key-auth": {"header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "jwe-fallback-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "jwe-fallback-key-user" {
			t.Fatalf("consumer_name = %v, want jwe-fallback-key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerAllowsKeyAuthAfterWolfRBACMissingToken(t *testing.T) {
	addAuthConsumer(t, "wolf-fallback-key-user", map[string]any{
		"key-auth": map[string]any{"key": "wolf-fallback-key"},
	})
	waitForConsumerKey(t, "key-auth", "wolf-fallback-key")

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"wolf-rbac": {}},
			{"key-auth": {"header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "wolf-fallback-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "wolf-fallback-key-user" {
			t.Fatalf("consumer_name = %v, want wolf-fallback-key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsWhenAllAuthPluginsFail(t *testing.T) {
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"basic-auth": {}},
			{"key-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", res.Code)
	}
	if !strings.Contains(res.Body.String(), "Authorization Failed") {
		t.Fatalf("body = %q, want Authorization Failed", res.Body.String())
	}
}

func TestPostInitRejectsUnsupportedAuthPlugin(t *testing.T) {
	p := &Plugin{config: Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {}},
			{"unknown-auth": {}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "unknown-auth") {
		t.Fatalf("PostInit() error = %v, want unknown-auth", err)
	}
}

func newMultiAuthRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithRequestVars(req)
	return req
}
