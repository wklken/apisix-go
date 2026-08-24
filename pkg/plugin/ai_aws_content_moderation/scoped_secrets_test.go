package ai_aws_content_moderation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
	hook   func(scopedSecretCall)
}

func (*scopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*scopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	call := scopedSecretCall{Scope: scope, Raw: raw}
	broker.mu.Lock()
	broker.calls = append(broker.calls, call)
	resolveErr := broker.fail[raw]
	value, found := broker.values[raw]
	hook := broker.hook
	broker.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if resolveErr != nil {
		return "", resolveErr
	}
	if found {
		return value, nil
	}
	return raw, nil
}

func (broker *scopedSecretBroker) callsSnapshot() []scopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]scopedSecretCall(nil), broker.calls...)
}

func (broker *scopedSecretBroker) resetCalls() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = nil
}

func (broker *scopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func (broker *scopedSecretBroker) setHook(hook func(scopedSecretCall)) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.hook = hook
}

func (*scopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

func newScopedSecretHarness(
	t *testing.T, factory string, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	return newScopedSecretHarnessAt(t, factory, 7, "r1", values)
}

func newScopedSecretHarnessAt(
	t *testing.T,
	factory string,
	revision uint64,
	resourceID string,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
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
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
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
	broker := &scopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	baseScope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     factory,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, baseScope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func awsScopedConfig(accessKeyID, secretAccessKey, sessionToken, endpoint string) Config {
	return Config{Comprehend: Comprehend{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		SessionToken:    sessionToken,
		Region:          "us-east-1",
		Endpoint:        endpoint,
	}}
}

func materializeScopedAWS(
	t *testing.T,
	p *Plugin,
	scope secret.Scope,
	capabilityValue secret.GenerationCapability,
) {
	t.Helper()
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func wantPluginConfigDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func assertAWSScopedCalls(
	t *testing.T, baseScope secret.Scope, calls []scopedSecretCall, fields []string, raws []string,
) {
	t.Helper()
	if len(calls) != len(fields) {
		t.Fatalf("broker calls = %d, want %d: %#v", len(calls), len(fields), calls)
	}
	for index, field := range fields {
		wantScope := baseScope
		wantScope.Field = field
		if calls[index].Scope != wantScope || calls[index].Raw != raws[index] {
			t.Fatalf(
				"broker call[%d] = %#v, want scope %#v and raw %q",
				index, calls[index], wantScope, raws[index],
			)
		}
	}
}

func assertAWSDescriptors(
	t *testing.T, p *Plugin, accessKeyID, secretAccessKey, sessionToken string,
) {
	t.Helper()
	wants := map[string]struct {
		got       string
		plaintext string
	}{
		"access key id":     {p.config.Comprehend.AccessKeyID, accessKeyID},
		"secret access key": {p.config.Comprehend.SecretAccessKey, secretAccessKey},
	}
	if sessionToken != "" {
		wants["session token"] = struct {
			got       string
			plaintext string
		}{p.config.Comprehend.SessionToken, sessionToken}
	} else if p.config.Comprehend.SessionToken != "" {
		t.Fatalf("session token config = %q, want empty", p.config.Comprehend.SessionToken)
	}
	for field, value := range wants {
		if want := wantPluginConfigDescriptor(value.plaintext); value.got != want {
			t.Fatalf("%s descriptor = %q, want %q", field, value.got, want)
		}
	}
}

func TestScopedSecretsMaterializeAWSComprehendCredentials(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_SCOPED_ACCESS_KEY"
		rawToken  = "$ENV://AWS_SCOPED_SESSION_TOKEN"
	)
	encryption := testutil.DataEncryptionService(true, []string{"0123456789abcdef"})
	rawSecret, err := encryption.EncryptForContext(
		"context-secret", name+".comprehend.secret_access_key",
	)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}

	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "Credential=resolved-access/") ||
			!strings.Contains(got, "x-amz-security-token") {
			t.Errorf("Authorization = %q, want resolved access and signed session token", got)
		}
		if got := r.Header.Get("X-Amz-Security-Token"); got != "resolved-token" {
			t.Errorf("X-Amz-Security-Token = %q, want resolved-token", got)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)

	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawAccess: "resolved-access",
		rawSecret: "context-secret",
		rawToken:  "resolved-token",
	})
	defer closeAttempt()
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, moderation.URL)}
	materializeScopedAWS(t, p, scope, capabilityValue)
	assertAWSScopedCalls(t, scope, broker.calls,
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token"},
		[]string{rawAccess, rawSecret, rawToken},
	)
	assertAWSDescriptors(t, p, "resolved-access", "context-secret", "resolved-token")
	for _, sensitive := range []string{
		rawAccess, rawSecret, rawToken, "AWS_SCOPED_ACCESS_KEY", "AWS_SCOPED_SESSION_TOKEN",
		"resolved-access", "context-secret", "resolved-token",
	} {
		config := fmt.Sprintf("%#v", p.config.Comprehend)
		if strings.Contains(config, sensitive) {
			t.Fatalf("effective config contains %q: %s", sensitive, config)
		}
	}

	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if _, err := p.detectToxicContent(
		httptest.NewRequest(http.MethodPost, "/", nil), "hello",
	); err != nil {
		t.Fatalf("detectToxicContent() error = %v", err)
	}
}

func TestScopedSecretsSkipEmptyAWSSessionToken(t *testing.T) {
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Security-Token"); got != "" {
			t.Errorf("X-Amz-Security-Token = %q, want empty", got)
		}
		if got := r.Header.Get("Authorization"); strings.Contains(got, "x-amz-security-token") {
			t.Errorf("Authorization = %q, unexpectedly signs an empty session token", got)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)

	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		"literal-access": "resolved-access",
		"literal-secret": "resolved-secret",
	})
	defer closeAttempt()
	p := &Plugin{config: awsScopedConfig("literal-access", "literal-secret", "", moderation.URL)}
	materializeScopedAWS(t, p, scope, capabilityValue)
	assertAWSScopedCalls(t, scope, broker.calls,
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key"},
		[]string{"literal-access", "literal-secret"},
	)
	assertAWSDescriptors(t, p, "resolved-access", "resolved-secret", "")
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if _, err := p.detectToxicContent(
		httptest.NewRequest(http.MethodPost, "/", nil), "hello",
	); err != nil {
		t.Fatalf("detectToxicContent() error = %v", err)
	}
}

func TestScopedSecretsResolveManagedAWSSessionToken(t *testing.T) {
	const rawToken = "$secret://vault/aws/session-token"
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Security-Token"); got != "managed-token" {
			t.Errorf("X-Amz-Security-Token = %q, want managed-token", got)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)

	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		"literal-access": "managed-access",
		"literal-secret": "managed-secret",
		rawToken:         "managed-token",
	})
	defer closeAttempt()
	p := &Plugin{config: awsScopedConfig("literal-access", "literal-secret", rawToken, moderation.URL)}
	materializeScopedAWS(t, p, scope, capabilityValue)
	assertAWSScopedCalls(t, scope, broker.calls,
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token"},
		[]string{"literal-access", "literal-secret", rawToken},
	)
	assertAWSDescriptors(t, p, "managed-access", "managed-secret", "managed-token")
	if strings.Contains(fmt.Sprintf("%#v", p.config), rawToken) {
		t.Fatalf("effective config contains managed token reference: %#v", p.config)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if _, err := p.detectToxicContent(
		httptest.NewRequest(http.MethodPost, "/", nil), "hello",
	); err != nil {
		t.Fatalf("detectToxicContent() error = %v", err)
	}
}

func TestScopedSecretsAWSFailureInstallsNothingAndSameInstanceRetries(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_RETRY_ACCESS_KEY"
		rawSecret = "$secret://vault/aws/retry-secret"
		rawToken  = "$secret://vault/aws/retry-token"
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawAccess: "retry-access",
		rawSecret: "retry-secret",
		rawToken:  "retry-token",
	})
	defer closeAttempt()
	broker.fail[rawSecret] = errors.New("test broker unavailable for " + rawSecret)
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, "http://127.0.0.1")}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil, want required credential failure")
	}
	for _, sensitive := range []string{rawAccess, rawSecret, rawToken, "test broker unavailable"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("MaterializeScopedPluginSecrets() error = %v, contains %q", err, sensitive)
		}
	}
	if p.config.Comprehend.AccessKeyID != rawAccess ||
		p.config.Comprehend.SecretAccessKey != rawSecret ||
		p.config.Comprehend.SessionToken != rawToken {
		t.Fatalf("config changed on failed scoped preparation: %#v", p.config.Comprehend)
	}
	assertAWSScopedCalls(t, scope, broker.calls,
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key"},
		[]string{rawAccess, rawSecret},
	)

	delete(broker.fail, rawSecret)
	broker.calls = nil
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	assertAWSScopedCalls(t, scope, broker.calls,
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token"},
		[]string{rawAccess, rawSecret, rawToken},
	)
	assertAWSDescriptors(t, p, "retry-access", "retry-secret", "retry-token")
}

type awsRequestCredentials struct {
	accessKeyID  string
	sessionToken string
}

func newScopedAWSPlugin(
	t *testing.T,
	endpoint string,
	revision uint64,
	resourceID string,
	accessKeyID string,
	secretAccessKey string,
	sessionToken string,
) (*Plugin, func()) {
	t.Helper()
	rawAccess := "$ENV://AWS_ACCESS_" + resourceID
	rawSecret := "$secret://vault/aws/secret-" + resourceID
	rawToken := "$secret://vault/aws/token-" + resourceID
	capabilityValue, scope, _, closeAttempt := newScopedSecretHarnessAt(
		t, name, revision, resourceID, map[string]string{
			rawAccess: accessKeyID,
			rawSecret: secretAccessKey,
			rawToken:  sessionToken,
		},
	)
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, endpoint)}
	materializeScopedAWS(t, p, scope, capabilityValue)
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p, closeAttempt
}

func TestScopedSecretsAWSGenerationInstancesDoNotCrossUseCredentials(t *testing.T) {
	requests := make(chan awsRequestCredentials, 2)
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		credential := ""
		if _, credentialPart, found := strings.Cut(authorization, "Credential="); found {
			credential, _, _ = strings.Cut(credentialPart, "/")
		}
		requests <- awsRequestCredentials{
			accessKeyID:  credential,
			sessionToken: r.Header.Get("X-Amz-Security-Token"),
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)

	p1, closeAttempt1 := newScopedAWSPlugin(
		t, moderation.URL, 11, "generation-n", "generation-n-access", "generation-n-secret", "generation-n-token",
	)
	p2, closeAttempt2 := newScopedAWSPlugin(
		t, moderation.URL, 12, "generation-n1", "generation-n1-access", "generation-n1-secret", "generation-n1-token",
	)
	defer closeAttempt1()
	defer closeAttempt2()
	t.Cleanup(p1.Stop)
	t.Cleanup(p2.Stop)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, plugin := range []*Plugin{p1, p2} {
		wg.Add(1)
		go func(plugin *Plugin) {
			defer wg.Done()
			_, err := plugin.detectToxicContent(
				httptest.NewRequest(http.MethodPost, "/", nil), "hello",
			)
			errs <- err
		}(plugin)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("detectToxicContent() error = %v", err)
		}
	}

	seen := make(map[awsRequestCredentials]bool)
	for range 2 {
		select {
		case got := <-requests:
			seen[got] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for AWS moderation request")
		}
	}
	for _, want := range []awsRequestCredentials{
		{accessKeyID: "generation-n-access", sessionToken: "generation-n-token"},
		{accessKeyID: "generation-n1-access", sessionToken: "generation-n1-token"},
	} {
		if !seen[want] {
			t.Fatalf("observed credentials = %#v, missing %#v", seen, want)
		}
	}
}

func TestScopedSecretsAWSStopWaitsForResponseAndIsIdempotent(t *testing.T) {
	responseStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var requests atomic.Int32
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(responseStarted)
		<-releaseResponse
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)

	p, closeAttempt := newScopedAWSPlugin(
		t, moderation.URL, 21, "stop-barrier", "stop-access", "stop-secret", "stop-token",
	)
	defer closeAttempt()
	drainStarted := make(chan struct{})
	var drainOnce sync.Once
	p.setAWSCredentialTestHooks(awsCredentialTestHooks{
		lifecycle: func(event awsCredentialLifecycleEvent) {
			if event == awsCredentialDrainStarted {
				drainOnce.Do(func() { close(drainStarted) })
			}
		},
	})
	requestDone := make(chan error, 1)
	go func() {
		_, err := p.detectToxicContent(httptest.NewRequest(http.MethodPost, "/", nil), "hello")
		requestDone <- err
	}()
	select {
	case <-responseStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for moderation response headers")
	}

	const stoppers = 16
	stopDone := make(chan struct{}, stoppers)
	for range stoppers {
		go func() {
			p.Stop()
			stopDone <- struct{}{}
		}()
	}
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		close(releaseResponse)
		<-requestDone
		t.Fatal("timed out waiting for Stop() to enter credential drain")
	}
	state := p.awsCredentialLifecycleSnapshot()
	if !state.retired || state.activeUses == 0 {
		close(releaseResponse)
		<-requestDone
		t.Fatalf("Stop() drain state = %#v, want retired with an active use", state)
	}
	select {
	case <-stopDone:
		close(releaseResponse)
		<-requestDone
		t.Fatal("concurrent Stop() returned while the credential-scoped response read was in flight")
	default:
	}
	close(releaseResponse)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight detectToxicContent() error = %v", err)
	}
	for range stoppers {
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent Stop() did not return after the response completed")
		}
	}
	state = p.awsCredentialLifecycleSnapshot()
	if state.scopedAccessKeyIDSet ||
		state.scopedSecretAccessKeySet ||
		state.scopedSessionTokenSet {
		t.Fatal("Stop() retained scoped credential values")
	}
	if state.scopedSet || state.scopedSessionTokenRawSet {
		t.Fatalf(
			"Stop() retained scoped mode flags: credentials=%v session_token=%v",
			state.scopedSet, state.scopedSessionTokenRawSet,
		)
	}

	p.Stop()
	_, err := p.detectToxicContent(httptest.NewRequest(http.MethodPost, "/", nil), "after stop")
	if err == nil {
		t.Fatal("detectToxicContent() after Stop() error = nil, want credential unavailable")
	}
	if strings.Contains(err.Error(), "stop-access") ||
		strings.Contains(err.Error(), "stop-secret") ||
		strings.Contains(err.Error(), "stop-token") {
		t.Fatalf("detectToxicContent() after Stop() error = %v, leaked credentials", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("moderation requests after Stop() = %d, want 1", got)
	}
}
