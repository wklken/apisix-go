package csrf

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func TestCSRFPluginSourceGuardRejectsDirectComparison(t *testing.T) {
	source, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("read plugin.go: %v", err)
	}
	if bytes.Contains(source, []byte("sign != csrfToken.Sign")) {
		t.Fatal("plugin.go compares token signatures with a direct non-constant-time expression")
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestAPISIXBehaviorUsesNonSecureCSRFCookie(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil),
	)
	if cookies := response.Result().Cookies(); len(cookies) != 1 || cookies[0].Secure {
		t.Fatalf("cookies = %#v, want one non-Secure cookie", cookies)
	}
}

func TestResponsePhaseGeneratesCSRFCookieAfterDownstreamCompletes(t *testing.T) {
	expires := int64(60)
	p := newTestPlugin(t, Config{Key: "secret", Expires: &expires})
	p.randomReader = bytes.NewReader(make([]byte, 8))
	requestTime := time.Unix(1700000000, 0).UTC()
	responseTime := requestTime.Add(30 * time.Second)
	current := requestTime
	p.now = func() time.Time { return current }

	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Fatalf("Set-Cookie before downstream completion = %#v", got)
		}
		current = responseTime
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/safe", nil))

	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie count = %d, want 1", len(cookies))
	}
	if !cookies[0].Expires.Equal(responseTime.Add(time.Duration(expires) * time.Second)) {
		t.Fatalf("cookie Expires = %s, want %s", cookies[0].Expires, responseTime.Add(time.Minute))
	}
	decoded, err := base64.StdEncoding.DecodeString(cookies[0].Value)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	var token csrfToken
	if err := json.Unmarshal(decoded, &token); err != nil {
		t.Fatalf("decode token JSON: %v", err)
	}
	if token.Expires != responseTime.Unix() {
		t.Fatalf("token timestamp = %d, want response time %d", token.Expires, responseTime.Unix())
	}
}

func TestExplicitResponseCallbacksIssueCSRFCookieForUpstreamAndEarlyStop(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	p.randomReader = bytes.NewReader(make([]byte, 16))
	p.now = func() time.Time { return time.Unix(1700000000, 0).UTC() }

	descriptor, err := p.config.DescribeBindingPhases()
	if err != nil {
		t.Fatalf("DescribeBindingPhases() error = %v", err)
	}
	if descriptor != (base.BindingPhaseDescriptor{RequestStage: "access", StreamingHeader: true}) {
		t.Fatalf("DescribeBindingPhases() = %#v", descriptor)
	}

	safeResponse := httptest.NewRecorder()
	safeRequest := httptest.NewRequest(http.MethodGet, "/safe", nil)
	result := p.RunRequestPhase(safeResponse, safeRequest)
	if result.Decision != base.RequestContinue {
		t.Fatalf("safe RunRequestPhase() = %#v", result)
	}
	safeRequest = result.Request
	if got := safeResponse.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("safe access phase Set-Cookie = %#v", got)
	}
	streaming := base.StreamingResponseState{Status: http.StatusOK, Header: make(http.Header)}
	if err := p.RunStreamingHeaderFilter(safeRequest, &streaming); err != nil {
		t.Fatalf("RunStreamingHeaderFilter() error = %v", err)
	}
	if got := streaming.Header.Values("Set-Cookie"); len(got) != 1 {
		t.Fatalf("streaming Set-Cookie = %#v, want one", got)
	}

	deniedResponse := httptest.NewRecorder()
	deniedRequest := httptest.NewRequest(http.MethodPost, "/protected", nil)
	if result := p.RunRequestPhase(deniedResponse, deniedRequest); result.Decision != base.RequestStop {
		t.Fatalf("denied RunRequestPhase() = %#v", result)
	}
	if got := deniedResponse.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("denied access phase Set-Cookie = %#v", got)
	}
	buffered := base.ResponseState{
		Status: deniedResponse.Code,
		Header: deniedResponse.Header().Clone(),
		Body:   deniedResponse.Body.Bytes(),
	}
	if err := p.RunBufferedBodyFilter(deniedRequest, &buffered); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if got := buffered.Header.Values("Set-Cookie"); len(got) != 1 {
		t.Fatalf("early-stop Set-Cookie = %#v, want one", got)
	}
}

type csrfScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type csrfScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []csrfScopedSecretCall
}

func (*csrfScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*csrfScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *csrfScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, csrfScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (*csrfScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

func newCSRFScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, raw string, resolved string,
) (secret.GenerationCapability, secret.Scope, *csrfScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"csrf":{}}}`),
	}}, nil)
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
			Key: key, Disposition: generation.DispositionPublished, Code: "csrf-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &csrfScopedSecretBroker{
		values: map[string]string{raw: resolved},
		fail:   make(map[string]error),
	}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func assertCSRFSecretDescriptor(t *testing.T, value string) {
	t.Helper()
	const prefix = "plugin_config#sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		t.Fatalf("csrf key = %q, want descriptor", value)
	}
}

func assertCSRFSecretDescriptorFor(t *testing.T, value, plaintext string) {
	t.Helper()
	assertCSRFSecretDescriptor(t, value)
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if value != want {
		t.Fatalf("csrf descriptor = %q, want exact admitted-plaintext digest %q", value, want)
	}
}

func materializeCSRFScopedPlugin(
	t *testing.T, revision uint64, resourceID, raw, resolved string,
) (*Plugin, *csrfScopedSecretBroker, func()) {
	t.Helper()
	capabilityValue, scope, broker, closeAttempt := newCSRFScopedSecretHarness(
		t, revision, resourceID, raw, resolved,
	)
	p := &Plugin{config: Config{Key: raw, Name: "csrf-token"}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p, broker, closeAttempt
}

func TestMaterializeScopedSecretsOwnsCSRFKey(t *testing.T) {
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext("contextual-key", "csrf.key")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "contextual ciphertext", raw: contextual, resolved: "contextual-key"},
		{name: "environment", raw: "$ENV://CSRF_KEY", resolved: "environment-key"},
		{name: "managed", raw: "$secret://vault/csrf-key", resolved: "managed-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, broker, closeAttempt := materializeCSRFScopedPlugin(
				t, 7, "csrf-route", tt.raw, tt.resolved,
			)
			defer closeAttempt()
			if len(broker.calls) != 1 {
				t.Fatalf("scoped calls = %#v, want one exact key call", broker.calls)
			}
			call := broker.calls[0]
			if call.Raw != tt.raw || call.Scope.Field != "key" ||
				call.Scope.Plugin != name || call.Scope.Source != capability.SecretPluginConfig ||
				call.Scope.Resource.ID != "csrf-route" {
				t.Fatalf("scoped call = %#v, want exact csrf.key authority", call)
			}
			assertCSRFSecretDescriptorFor(t, p.config.Key, tt.resolved)
			if strings.Contains(p.config.Key, tt.raw) || strings.Contains(p.config.Key, tt.resolved) {
				t.Fatalf("public config retained secret material: %q", p.config.Key)
			}
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/safe", nil))
			if response.Code != http.StatusNoContent || len(response.Result().Cookies()) != 1 {
				t.Fatalf(
					"safe request status/cookies = %d/%d, want 204/1",
					response.Code,
					len(response.Result().Cookies()),
				)
			}
			token, err := genCSRFToken(tt.resolved, bytes.NewReader(make([]byte, 8)))
			if err != nil {
				t.Fatalf("genCSRFToken() error = %v", err)
			}
			request := httptest.NewRequest(http.MethodPost, "/protected", nil)
			request.Header.Set("csrf-token", token)
			request.AddCookie(&http.Cookie{Name: "csrf-token", Value: token})
			protected := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(protected, request)
			if protected.Code != http.StatusNoContent {
				t.Fatalf("protected request status = %d, want 204", protected.Code)
			}
		})
	}
}

func TestPostInitWithoutMaterializationCannotUseKey(t *testing.T) {
	p := &Plugin{config: Config{Key: "$ENV://CSRF_UNPREPARED"}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err == nil || !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want unavailable credential", err)
	}
	if p.config.Key != "$ENV://CSRF_UNPREPARED" {
		t.Fatalf("unprepared PostInit changed public key = %q", p.config.Key)
	}
}

func TestMaterializeScopedSecretsFailureIsAtomicAndRedacted(t *testing.T) {
	const raw = "$secret://vault/csrf-failure"
	capabilityValue, scope, broker, closeAttempt := newCSRFScopedSecretHarness(
		t, 7, "csrf-failure", raw, "private-key",
	)
	defer closeAttempt()
	broker.fail[raw] = errors.New("private resolver failure")
	p := &Plugin{config: Config{Key: raw}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-key") ||
		strings.Contains(err.Error(), "private resolver") {
		t.Fatalf("materialization error leaked secret details: %v", err)
	}
	if p.config.Key != raw || p.key != nil {
		t.Fatalf("failed materialization retained state: config=%q key=%#v", p.config.Key, p.key)
	}
}

func TestMaterializeScopedSecretsRejectsEmptyAdmittedKeyAndRetriesAtomically(t *testing.T) {
	const raw = "$ENV://CSRF_EMPTY"
	for _, resolved := range []string{"", "   "} {
		t.Run(fmt.Sprintf("resolved-%q", resolved), func(t *testing.T) {
			capabilityValue, scope, broker, closeAttempt := newCSRFScopedSecretHarness(
				t, 9, "csrf-empty", raw, resolved,
			)
			defer closeAttempt()
			p := &Plugin{config: Config{Key: raw}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if err == nil {
				t.Fatal("empty admitted key materialized successfully")
			}
			if strings.Contains(err.Error(), raw) ||
				(resolved != "" && strings.Contains(err.Error(), resolved)) {
				t.Fatalf("empty-key error leaked secret details: %v", err)
			}
			if p.config.Key != raw || p.key != nil || p.legacyKey != nil {
				t.Fatalf(
					"failed empty-key materialization retained state: config=%q key=%#v legacy=%p",
					p.config.Key,
					p.key,
					p.legacyKey,
				)
			}

			broker.mu.Lock()
			broker.values[raw] = "retry-key"
			broker.mu.Unlock()
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
				t.Fatalf("retry materialization error = %v", err)
			}
			assertCSRFSecretDescriptorFor(t, p.config.Key, "retry-key")
			if err := p.PostInit(); err != nil {
				t.Fatalf("PostInit() after retry error = %v", err)
			}
			var got string
			if err := p.useKey(func(value string) error {
				got = value
				return nil
			}); err != nil || got != "retry-key" {
				t.Fatalf("retried private key = %q, err = %v, want retry-key", got, err)
			}
		})
	}
}

func TestMaterializeScopedSecretsIsIdempotent(t *testing.T) {
	const raw = "$ENV://CSRF_IDEMPOTENT"
	capabilityValue, scope, broker, closeAttempt := newCSRFScopedSecretHarness(
		t, 8, "csrf-idempotent", raw, "idempotent-key",
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Key: raw}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	descriptor := p.config.Key
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	if len(broker.calls) != 1 || p.config.Key != descriptor {
		t.Fatalf(
			"idempotent materialization calls/config = %d/%q, want 1/%q",
			len(broker.calls),
			p.config.Key,
			descriptor,
		)
	}
}

func TestCSRFKeyRotationDoesNotCrossGenerationsAndStopIsIdempotent(t *testing.T) {
	pN, _, closeN := materializeCSRFScopedPlugin(t, 11, "csrf-n", "$ENV://CSRF_N", "key-n")
	defer closeN()
	pN1, _, closeN1 := materializeCSRFScopedPlugin(t, 12, "csrf-n1", "$ENV://CSRF_N1", "key-n1")
	defer closeN1()

	request := func(p *Plugin) string {
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/safe", nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("safe request status = %d, want 204", response.Code)
		}
		cookies := response.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("safe request cookies = %#v, want one", cookies)
		}
		return cookies[0].Value
	}
	tokenN := request(pN)
	tokenN1 := request(pN1)
	if !checkCSRFToken(tokenN, "key-n", pN.expires()) ||
		!checkCSRFToken(tokenN1, "key-n1", pN1.expires()) {
		t.Fatal("generation tokens did not verify with their own private keys")
	}
	if checkCSRFToken(tokenN, "key-n1", pN1.expires()) || checkCSRFToken(tokenN1, "key-n", pN.expires()) {
		t.Fatal("generation tokens crossed private key boundaries")
	}

	pN.Stop()
	pN.Stop()
	if pN.key != nil || pN.legacyKey != nil {
		t.Fatalf("generation N secret state after Stop = key:%#v legacy:%p", pN.key, pN.legacyKey)
	}
	if got := request(pN1); got == "" {
		t.Fatal("generation N+1 stopped producing csrf tokens after generation N retirement")
	}
}

func TestCSRFHandlerAndStopAreSafeConcurrently(t *testing.T) {
	p, _, closeAttempt := materializeCSRFScopedPlugin(
		t, 13, "csrf-concurrent", "$ENV://CSRF_CONCURRENT", "concurrent-key",
	)
	defer closeAttempt()

	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/safe", nil))
		})
	}
	group.Go(p.Stop)
	group.Go(p.Stop)
	group.Wait()
}

func TestCSRFRequestHoldsValueUseWhileStopWaits(t *testing.T) {
	p, _, closeAttempt := materializeCSRFScopedPlugin(
		t, 14, "csrf-barrier", "$ENV://CSRF_BARRIER", "barrier-key",
	)
	defer closeAttempt()

	entered := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseRequest()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	go func() {
		response := httptest.NewRecorder()
		p.Handler(next).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/barrier", nil))
		if response.Code != http.StatusNoContent {
			t.Errorf("barrier request status = %d, want 204", response.Code)
		}
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request to enter complete verify-or-generate callback")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while request still held Value.Use read lock")
	case <-time.After(100 * time.Millisecond):
	}

	releaseRequest()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for barrier request after release")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Stop after request release")
	}

	retired := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired csrf plugin called next handler")
	})).ServeHTTP(retired, httptest.NewRequest(http.MethodGet, "/retired", nil))
	if retired.Code != http.StatusInternalServerError ||
		strings.TrimSpace(retired.Body.String()) != `{"error_msg":"csrf key unavailable"}` {
		t.Fatalf("retired request = %d/%q, want 500/key unavailable", retired.Code, retired.Body.String())
	}
	p.Stop()
}

func TestMaterializeSecretsRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{}
	if err := p.MaterializeSecrets(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("MaterializeSecrets() error = %v, want missing resolver error", err)
	}
}

func TestHandlerRejectsMissingHeaderWithJSONError(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX response type", got)
	}
	if got := rr.Body.String(); got != "{\"error_msg\":\"no csrf token in headers\"}\n" {
		t.Fatalf("body = %q, want APISIX csrf error JSON", got)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "apisix-csrf-token" ||
		cookies[0].Path != "/" || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookies = %#v, want one APISIX CSRF refresh cookie", cookies)
	}
}

func TestPostInitRejectsInvalidEncryptedKey(t *testing.T) {
	p := &Plugin{config: Config{Key: "plain"}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want strict encrypted csrf key rejection")
	}
}

func TestPostInitRejectsEmptyKey(t *testing.T) {
	for _, key := range []string{"", "   "} {
		p := &Plugin{config: Config{Key: key}}
		p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
		if err := p.Init(); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if err := p.MaterializeSecrets(); err == nil {
			t.Fatalf("MaterializeSecrets() error = nil for key %q, want empty key rejection", key)
		}
	}
}

func TestPostInitResolvesEncryptedKey(t *testing.T) {
	key := "qeddd145sfvddff3"

	p := &Plugin{config: Config{Key: encryptCSRFTestValue(t, key, "secret")}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(true, []string{key}).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	assertCSRFSecretDescriptorFor(t, p.config.Key, "secret")
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	var got string
	if err := p.useKey(func(value string) error {
		got = value
		return nil
	}); err != nil || got != "secret" {
		t.Fatalf("private csrf key = %q, err = %v, want decrypted value", got, err)
	}
}

func TestPostInitResolvesKeyFromRotatedKeyring(t *testing.T) {
	oldKey := "qeddd145sfvddff3"
	newKey := "1234567890abcdef"

	p := &Plugin{config: Config{Key: encryptCSRFTestValue(t, oldKey, "rotated-secret")}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	assertCSRFSecretDescriptorFor(t, p.config.Key, "rotated-secret")
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	var got string
	if err := p.useKey(func(value string) error {
		got = value
		return nil
	}); err != nil || got != "rotated-secret" {
		t.Fatalf("private csrf key = %q, err = %v, want rotated plaintext", got, err)
	}
}

func TestPostInitRejectsMissingKeyring(t *testing.T) {
	p := &Plugin{config: Config{Key: "ciphertext"}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(true, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want missing keyring rejection")
	}
}

func encryptCSRFTestValue(t *testing.T, key string, value string) string {
	t.Helper()

	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func TestCheckCSRFTokenAllowsExpiredTokenWhenExpiresIsZero(t *testing.T) {
	key := "secret"
	token := csrfToken{
		Random:  0.25,
		Expires: 1,
	}
	token.Sign = genSign(token.Random, token.Expires, key)
	body, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}

	if !checkCSRFToken(base64.StdEncoding.EncodeToString(body), key, 0) {
		t.Fatal("checkCSRFToken() = false, want true when expires is zero")
	}
}

func TestGenCSRFTokenUsesInjectedReader(t *testing.T) {
	reader := bytes.NewReader(bytes.Repeat([]byte{0xff}, 8))
	tokenValue, err := genCSRFToken("secret", reader)
	if err != nil {
		t.Fatalf("genCSRFToken() error = %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(tokenValue)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	var token csrfToken
	if err := json.Unmarshal(decoded, &token); err != nil {
		t.Fatalf("unmarshal token: %v", err)
	}

	wantRandom := float64((uint64(1)<<53)-1) / float64(uint64(1)<<53)
	if token.Random != wantRandom {
		t.Fatalf("random = %.17g, want %.17g", token.Random, wantRandom)
	}
	if token.Random < 0 || token.Random >= 1 {
		t.Fatalf("random = %.17g, want value in [0, 1)", token.Random)
	}
	if token.Sign != genSign(token.Random, token.Expires, "secret") {
		t.Fatal("generated token signature does not match its serialized fields")
	}
}

func TestHandlerFailsClosedAfterDownstreamWhenResponsePhaseEntropyFails(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
	p.randomReader = bytes.NewReader(nil)

	response := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/safe", nil))

	if !called {
		t.Fatal("next handler did not run before response-phase csrf entropy failure")
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"error_msg":"failed to generate csrf token"}` {
		t.Fatalf("body = %q, want generic csrf generation error", got)
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("response set a csrf cookie after entropy failure")
	}
}

func TestPostInitPreservesExplicitZeroExpires(t *testing.T) {
	p := newTestPlugin(t, Config{
		Key:     "secret",
		Expires: new(int64(0)),
	})

	if got := p.expires(); got != 0 {
		t.Fatalf("expires = %d, want explicit zero preserved", got)
	}
}

func TestCheckCSRFTokenValidationTable(t *testing.T) {
	key := "secret"
	now := time.Now().Unix()
	valid := csrfToken{Random: 0.25, Expires: now, Sign: genSign(0.25, now, key)}
	validBody, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal valid token: %v", err)
	}

	expired := csrfToken{Random: 0.5, Expires: now - 7300, Sign: genSign(0.5, now-7300, key)}
	expiredBody, err := json.Marshal(expired)
	if err != nil {
		t.Fatalf("marshal expired token: %v", err)
	}

	wrongKey := csrfToken{Random: 0.75, Expires: now, Sign: genSign(0.75, now, "other-key")}
	wrongKeyBody, err := json.Marshal(wrongKey)
	if err != nil {
		t.Fatalf("marshal wrong-signature token: %v", err)
	}

	shortSign := csrfToken{Random: 0.8, Expires: now, Sign: "short-signature"}
	shortSignBody, err := json.Marshal(shortSign)
	if err != nil {
		t.Fatalf("marshal short-signature token: %v", err)
	}

	longSign := csrfToken{Random: 0.9, Expires: now, Sign: strings.Repeat("a", 128)}
	longSignBody, err := json.Marshal(longSign)
	if err != nil {
		t.Fatalf("marshal long-signature token: %v", err)
	}

	tests := []struct {
		name      string
		token     string
		key       string
		expires   int64
		wantValid bool
	}{
		{
			name:      "valid signature",
			token:     base64.StdEncoding.EncodeToString(validBody),
			key:       key,
			expires:   7200,
			wantValid: true,
		},
		{name: "invalid base64", token: "!!!not-base64!!!", key: key, expires: 7200},
		{name: "invalid json", token: base64.StdEncoding.EncodeToString([]byte("{not json")), key: key, expires: 7200},
		{name: "expired timestamp", token: base64.StdEncoding.EncodeToString(expiredBody), key: key, expires: 7200},
		{name: "wrong signature", token: base64.StdEncoding.EncodeToString(wrongKeyBody), key: key, expires: 7200},
		{
			name:    "wrong signature shorter",
			token:   base64.StdEncoding.EncodeToString(shortSignBody),
			key:     key,
			expires: 7200,
		},
		{
			name:    "wrong signature longer",
			token:   base64.StdEncoding.EncodeToString(longSignBody),
			key:     key,
			expires: 7200,
		},
		{
			name:      "expires zero bypass",
			token:     base64.StdEncoding.EncodeToString(expiredBody),
			key:       key,
			expires:   0,
			wantValid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := checkCSRFToken(test.token, test.key, test.expires); got != test.wantValid {
				t.Fatalf("checkCSRFToken() = %t, want %t", got, test.wantValid)
			}
		})
	}
}

func TestHandlerValidPostRefreshesCookie(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
	token, err := genCSRFToken("secret", bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Fatalf("genCSRFToken() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
	request.Header.Set("csrf-token", token)
	request.AddCookie(&http.Cookie{Name: "csrf-token", Value: token})

	called := false
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called/status = %t/%d, want true/204", called, response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "csrf-token" || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v, want name csrf-token, path /, SameSite Lax", cookie)
	}
}

func TestHandlerRejectsInvalidRequestsWithJSONErrors(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		cookie   string
		wantBody string
	}{
		{name: "missing cookie", header: "token", wantBody: `{"error_msg":"no csrf cookie"}`},
		{
			name:     "mismatch",
			header:   "header-token",
			cookie:   "cookie-token",
			wantBody: `{"error_msg":"csrf token mismatch"}`,
		},
		{
			name:     "invalid signature",
			header:   "forged",
			cookie:   "forged",
			wantBody: `{"error_msg":"Failed to verify the csrf token signature"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
			request := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
			request.Header.Set("csrf-token", test.header)
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: "csrf-token", Value: test.cookie})
			}

			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("next handler should not run for an invalid token")
			})).ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
			if got := strings.TrimSpace(response.Body.String()); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
		})
	}
}

func TestHandlerSafeMethodsSetNewCookieAndContinue(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
			called := false
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(method, "http://example.com/safe", nil))

			if !called || response.Code != http.StatusNoContent {
				t.Fatalf("called/status = %t/%d, want true/204", called, response.Code)
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != "csrf-token" {
				t.Fatalf("cookies = %#v, want one refreshed csrf-token cookie", cookies)
			}
		})
	}
}

func TestPluginConfigDefaultsAndIdentity(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	if got := p.Config(); got != &p.config {
		t.Fatal("Config() does not return the plugin config pointer")
	}
	if p.config.Name != "apisix-csrf-token" {
		t.Fatalf("default name = %q, want apisix-csrf-token", p.config.Name)
	}
	if p.expires() != defaultCSRFExpires {
		t.Fatalf("default expires = %d, want %d", p.expires(), defaultCSRFExpires)
	}
	if p.randomReader == nil {
		t.Fatal("random reader is nil after PostInit()")
	}
}
