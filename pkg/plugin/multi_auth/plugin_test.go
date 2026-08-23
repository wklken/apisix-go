package multi_auth

import (
	"bytes"
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
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/hmac_auth"
	"github.com/wklken/apisix-go/pkg/plugin/key_auth"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s, err := store.GetStore(t.TempDir()+"/multi-auth.db", testEvents, testutil.DataEncryptionService(false, nil))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
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

func TestPostInitDispatchesConfiguredBodyConfigs(t *testing.T) {
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"key-auth": {"header": "apikey", "query": "api_key"}},
			{"hmac-auth": {
				"access_key":            "route-ak",
				"secret_key":            "route-sk",
				"validate_request_body": true,
				"max_req_body_size":     4096,
			}},
		},
	})

	if len(p.auths) != 2 {
		t.Fatalf("configured auth plugins = %d, want 2", len(p.auths))
	}
	keyAuth, ok := p.auths[0].plugin.Config().(*key_auth.Config)
	if !ok {
		t.Fatalf("first auth config = %T, want *key_auth.Config", p.auths[0].plugin.Config())
	}
	if keyAuth.Header != "apikey" || keyAuth.Query != "api_key" {
		t.Fatalf("key-auth config = %#v, want the route-level header/query", keyAuth)
	}
	hmacAuth, ok := p.auths[1].plugin.Config().(*hmac_auth.Config)
	if !ok {
		t.Fatalf("second auth config = %T, want *hmac_auth.Config", p.auths[1].plugin.Config())
	}
	if !hmacAuth.ValidateRequestBody || hmacAuth.MaxReqBodySize != 4096 {
		t.Fatalf("hmac-auth config = %#v, want the route-level body validation", hmacAuth)
	}
}

func TestMultiAuthRejectsDisabledNestedPluginBeforeConstruction(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"unknown-auth": {}},
		{"key-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetPluginEnabledChecker(func(name string) bool { return name != "unknown-auth" })

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "unknown-auth") || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("PostInit() error = %v, want disabled unknown-auth rejection before construction", err)
	}
	if len(p.auths) != 0 {
		t.Fatalf("configured auth plugins after rejection = %d, want no child constructed", len(p.auths))
	}

	enabled := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}}}
	if err := enabled.Init(); err != nil {
		t.Fatalf("enabled Init() error = %v", err)
	}
	enabled.SetPluginEnabledChecker(func(string) bool { return true })
	if err := enabled.PostInit(); err != nil {
		t.Fatalf("enabled PostInit() error = %v", err)
	}
	if len(enabled.auths) != 2 {
		t.Fatalf("enabled auth plugins = %d, want 2", len(enabled.auths))
	}
}

type bodyIsolatingAuthConfig struct {
	ValidateRequestBody bool
	MaxReqBodySize      int64
}

func (c *bodyIsolatingAuthConfig) BodyIsolation() (bool, int64) {
	return c.ValidateRequestBody, c.MaxReqBodySize
}

type bodyIsolatingAuthPlugin struct {
	config *bodyIsolatingAuthConfig
}

func (p *bodyIsolatingAuthPlugin) Init() error                            { return nil }
func (p *bodyIsolatingAuthPlugin) PostInit() error                        { return nil }
func (p *bodyIsolatingAuthPlugin) Config() any                            { return p.config }
func (p *bodyIsolatingAuthPlugin) GetSchema() string                      { return "" }
func (p *bodyIsolatingAuthPlugin) Handler(next http.Handler) http.Handler { return next }

func TestConfiguredBodyIsolationDispatchesByInterface(t *testing.T) {
	auth := configuredAuth{
		name: "body-aware",
		plugin: &bodyIsolatingAuthPlugin{config: &bodyIsolatingAuthConfig{
			ValidateRequestBody: true,
			MaxReqBodySize:      128,
		}},
	}
	original := httptest.NewRequest(http.MethodPost, "http://example.test/login", strings.NewReader("payload"))
	probe := original.Clone(original.Context())

	state := auth.isolateRequestBody(original, probe)
	if state == nil {
		t.Fatal("isolateRequestBody() = nil, want interface-dispatched body isolation")
	}

	consumed, err := io.ReadAll(probe.Body)
	if err != nil {
		t.Fatalf("read probe body: %v", err)
	}
	if string(consumed) != "payload" {
		t.Fatalf("probe body = %q, want payload", consumed)
	}
	state.restore(original)
	restored, err := io.ReadAll(original.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != "payload" {
		t.Fatalf("restored body = %q, want the original payload intact", restored)
	}
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
	res := httptest.NewRecorder()
	downstreamCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamCalls++
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "rejecting-consumer-user" {
			t.Fatalf("consumer_name = %v, want rejecting-consumer-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if downstreamCalls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstreamCalls)
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("response = %d, want 204", res.Code)
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
	res := httptest.NewRecorder()
	downstreamCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if downstreamCalls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstreamCalls)
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
}

func TestHandlerPassesAuthenticatedRequestToDownstream(t *testing.T) {
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
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "context-consumer-user" {
			t.Fatalf("consumer_name = %v, want context-consumer-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", res.Code)
	}
}

func TestBasicAuthDiagnosticDoesNotExposeCredentials(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	stop := logger.ReplaceObserver("basic-auth-redaction", func(entry logger.Entry) {
		mu.Lock()
		messages = append(messages, entry.Message)
		mu.Unlock()
	})
	t.Cleanup(stop)

	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"basic-auth": {}},
			{"key-auth": {}},
		},
	})
	req := newMultiAuthRequest()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("alicesecret")))
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", res.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, message := range messages {
		for _, secret := range []string{"alicesecret", "alice", "secret"} {
			if strings.Contains(message, secret) {
				t.Fatalf("log message %q exposes %q", message, secret)
			}
		}
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
	source := &countingReadCloser{Reader: strings.NewReader(body)}
	req := httptest.NewRequest(http.MethodPost, "/body", strings.NewReader(body))
	req.Body = source
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithRequestVars(req)
	req.Header.Set("apikey", "body-api-key")
	req.Header.Set("Digest", "SHA-256=unused")
	setTestHMACSignature(req, "body-hmac-key", "body-hmac-secret")
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
	if source.closeCalls != 0 {
		t.Fatalf("source body close calls before server ownership ends = %d, want 0", source.closeCalls)
	}
	if err := req.Body.Close(); err != nil {
		t.Fatalf("close final request body: %v", err)
	}
	if source.closeCalls != 1 {
		t.Fatalf("source body close calls after final close = %d, want 1", source.closeCalls)
	}
}

func TestHandlerLeavesSuccessfulHMACBodyOwnedByServer(t *testing.T) {
	addAuthConsumer(t, "body-hmac-success-user", map[string]any{
		"hmac-auth": map[string]any{"key_id": "success-hmac-key", "secret_key": "success-hmac-secret"},
	})
	waitForConsumerKey(t, "hmac-auth", "success-hmac-key")

	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"hmac-auth": {"validate_request_body": true, "max_req_body_size": 10}},
		{"key-auth": {}},
	}})
	body := "small"
	source := &countingReadCloser{Reader: strings.NewReader(body)}
	req := httptest.NewRequest(http.MethodPost, "/body", strings.NewReader(body))
	req.Body = source
	req = ctx.WithApisixVars(req, map[string]string{})
	req = ctx.WithRequestVars(req)
	digest := sha256.Sum256([]byte(body))
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(digest[:]))
	setTestHMACSignature(req, "success-hmac-key", "success-hmac-secret")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Fatalf("downstream body = %q, err=%v; want %q", got, err, body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
	if source.closeCalls != 0 {
		t.Fatalf("source body close calls before server ownership ends = %d, want 0", source.closeCalls)
	}
	if err := req.Body.Close(); err != nil {
		t.Fatalf("close server-owned request body: %v", err)
	}
	if source.closeCalls != 1 {
		t.Fatalf("source body close calls after final close = %d, want 1", source.closeCalls)
	}
}

func TestPostInitAllowsAuthPluginEntryWithMultiplePlugins(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}, "key-auth": {}},
		{"jwt-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want all plugins in an entry to be accepted", err)
	}
	if len(p.auths) != 3 {
		t.Fatalf("configured auth plugins = %d, want 3", len(p.auths))
	}
}

func TestHandlerRunsEveryAuthPluginWithinArrayObject(t *testing.T) {
	addAuthConsumer(t, "same-entry-key-user", map[string]any{
		"key-auth": map[string]any{"key": "same-entry-key"},
	})
	waitForConsumerKey(t, "key-auth", "same-entry-key")

	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}, "key-auth": {"header": "apikey"}},
		{"jwt-auth": {}},
	}})
	req := newMultiAuthRequest()
	req.Header.Set("apikey", "same-entry-key")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "same-entry-key-user" {
			t.Fatalf("consumer_name = %v, want same-entry-key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
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

func TestSuccessfulDirectAuthDoesNotLeakProbeRecorderContext(t *testing.T) {
	req := newMultiAuthRequest()
	authenticated, failure := (configuredAuth{name: "direct-success-auth", plugin: directSuccessAuth{}}).succeeds(req)
	if authenticated == nil || failure.name != "" {
		t.Fatalf(
			"direct success result = (%v, %+v), want authenticated request without failure",
			authenticated,
			failure,
		)
	}
	if authenticated.Header.Get("X-Direct-Auth") != "authenticated" {
		t.Fatalf("authenticated header = %q, want preserved mutation", authenticated.Header.Get("X-Direct-Auth"))
	}
	if ctx.RecordAuthProbeDiagnostic(authenticated, "must not be captured") {
		t.Fatal("successful request leaked the auth-probe diagnostic recorder")
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

func TestFailureDiagnosticTruncationBoundaries(t *testing.T) {
	const limit = maxFailureDiagnosticBytes
	long := strings.Repeat("x", limit+1024)

	t.Run("below limit", func(t *testing.T) {
		var buffer bytes.Buffer
		appendFailureDiagnostic(&buffer, "short")
		if got := buffer.String(); got != "short" {
			t.Fatalf("diagnostic = %q, want short", got)
		}
	})
	t.Run("exactly at limit", func(t *testing.T) {
		var buffer bytes.Buffer
		appendFailureDiagnostic(&buffer, long[:limit])
		if buffer.Len() != limit {
			t.Fatalf("diagnostic bytes = %d, want %d", buffer.Len(), limit)
		}
	})
	t.Run("above limit single message", func(t *testing.T) {
		var buffer bytes.Buffer
		appendFailureDiagnostic(&buffer, long)
		if buffer.Len() != limit {
			t.Fatalf("diagnostic bytes = %d, want %d", buffer.Len(), limit)
		}
	})
	t.Run("above limit across messages", func(t *testing.T) {
		var buffer bytes.Buffer
		appendFailureDiagnostic(&buffer, strings.Repeat("a", limit/2))
		appendFailureDiagnostic(&buffer, strings.Repeat("b", limit/2+64))
		if buffer.Len() != limit {
			t.Fatalf("diagnostic bytes = %d, want %d", buffer.Len(), limit)
		}
	})
	t.Run("separator split point", func(t *testing.T) {
		var buffer bytes.Buffer
		appendFailureDiagnostic(&buffer, strings.Repeat("a", limit-1))
		appendFailureDiagnostic(&buffer, "b")
		if buffer.Len() != limit {
			t.Fatalf("diagnostic bytes = %d, want %d", buffer.Len(), limit)
		}
		if got := buffer.String(); !strings.HasSuffix(got, ";") {
			t.Fatalf("diagnostic = %q, want it to end at the separator split point", got)
		}
	})
}

func TestProbeWriterMatchesAppendFailureTruncation(t *testing.T) {
	message := strings.Repeat("y", maxFailureDiagnosticBytes-10)

	writer := &probeResponseWriter{header: http.Header{}}
	if _, err := writer.Write([]byte(message)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var buffer bytes.Buffer
	appendFailureDiagnostic(&buffer, message)

	if writer.body.String() != buffer.String() {
		t.Fatalf(
			"probe writer diagnostic = %q, append diagnostic = %q, want identical bounded output",
			writer.body.String(),
			buffer.String(),
		)
	}
}

type statusOnlyAuth struct{}

type directSuccessAuth struct{}

type countingReadCloser struct {
	io.Reader
	closeCalls int
}

func (r *countingReadCloser) Close() error {
	r.closeCalls++
	return nil
}

func setTestHMACSignature(req *http.Request, keyID string, secret string) {
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	signing := keyID + "\n" + req.Method + " " + req.URL.RequestURI() + "\ndate: " + date + "\n"
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signing))
	req.Header.Set(
		"Authorization",
		`Signature keyId="`+keyID+`",algorithm="hmac-sha256",headers="@request-target date",signature="`+
			base64.StdEncoding.EncodeToString(mac.Sum(nil))+`"`,
	)
}

func (statusOnlyAuth) Init() error       { return nil }
func (statusOnlyAuth) PostInit() error   { return nil }
func (statusOnlyAuth) Config() any       { return &struct{}{} }
func (statusOnlyAuth) GetSchema() string { return `{}` }
func (statusOnlyAuth) Handler(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
}

func (directSuccessAuth) Init() error       { return nil }
func (directSuccessAuth) PostInit() error   { return nil }
func (directSuccessAuth) Config() any       { return &struct{}{} }
func (directSuccessAuth) GetSchema() string { return `{}` }
func (directSuccessAuth) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Direct-Auth", "authenticated")
		next.ServeHTTP(
			w,
			ctx.WithAuthenticationState(r, ctx.NewAuthenticationState("direct-success-auth", resource.Consumer{})),
		)
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

func TestHandlerRejectsJWEDecryptPassthroughWithoutAuthenticationState(t *testing.T) {
	p := newTestPlugin(t, Config{
		AuthPlugins: []AuthPluginConfig{
			{"jwe-decrypt": {"header": "Authorization", "forward_header": "Authorization", "strict": false}},
			{"key-auth": {"header": "apikey"}},
		},
	})
	req := newMultiAuthRequest()
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called without authentication state")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401; body=%s", res.Code, res.Body.String())
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

func TestUnownedSecretReferenceRejectsNestedAuthPlugin(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {"realm": "$ENV://MULTI_AUTH_REALM"}},
		{"key-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.PostInit()
	if err == nil ||
		!strings.Contains(err.Error(), "unowned secret reference") ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("PostInit() error = %v, want nested auth secret rejection", err)
	}
}
