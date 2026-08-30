package ai_proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type aiProxyScopedSecretCall struct {
	scope secret.Scope
	raw   string
}

type aiProxyScopedSecretBroker struct {
	values map[string]string
	calls  []aiProxyScopedSecretCall
}

type aiProxyCloseRecordingTransport struct {
	closed    chan struct{}
	closeOnce sync.Once
	closes    atomic.Int32
}

func (*aiProxyCloseRecordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected AI proxy request")
}

func (transport *aiProxyCloseRecordingTransport) CloseIdleConnections() {
	transport.closes.Add(1)
	transport.closeOnce.Do(func() { close(transport.closed) })
}

func (broker *aiProxyScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.calls = append(broker.calls, aiProxyScopedSecretCall{scope: scope, raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing scoped AI proxy credential")
	}
	return value, nil
}

func newAIProxyScopedSecretHarness(
	t *testing.T, factory string, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *aiProxyScopedSecretBroker, func()) {
	t.Helper()
	const revision = uint64(121)
	key := generation.ResourceKey{Kind: "routes", ID: factory + "-scoped"}
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
			Key: key, Disposition: generation.DispositionPublished, Code: "ai-proxy-scoped-test",
		}},
	}
	publication := generation.PublicationSet{
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
	broker := &aiProxyScopedSecretBroker{values: values}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision, Domain: generation.DomainHTTP,
		Plugin: factory, Resource: key, Source: capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close scoped AI proxy generation: %v", err)
		}
	}
}

func aiProxySecretDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func TestMaterializeScopedSecretsOwnsAIProxyCredentials(t *testing.T) {
	const (
		headerRaw  = "$ENV://AI_PROXY_HEADER"
		queryRaw   = "$secret://ai/query"
		gcpRaw     = "$secret://ai/gcp"
		awsRaw     = "$ENV://AI_PROXY_AWS_SECRET"
		sessionRaw = "$ENV://AI_PROXY_AWS_SESSION"
	)
	values := map[string]string{
		headerRaw: "Bearer resolved-header", queryRaw: "resolved-query",
		gcpRaw: `{"client_email":"resolved@example.com"}`,
		awsRaw: "resolved-aws-secret", sessionRaw: "resolved-session",
	}
	secrets, scope, broker, closeAttempt := newAIProxyScopedSecretHarness(t, name, values)
	defer closeAttempt()
	p := &Plugin{config: Config{Auth: Auth{
		Header: map[string]string{"Authorization": headerRaw},
		Query:  map[string]string{"api-key": queryRaw},
		GCP:    &ai_auth.GCPConfig{ServiceAccountJSON: gcpRaw, MaxTTL: 30},
		AWS: &ai_auth.AWSConfig{
			AccessKeyID: "public-access-key", SecretAccessKey: awsRaw, SessionToken: sessionRaw,
		},
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	wantFields := []string{
		"auth.header", "auth.query", "auth.gcp.service_account_json",
		"auth.aws.secret_access_key", "auth.aws.session_token",
	}
	wantRaw := []string{headerRaw, queryRaw, gcpRaw, awsRaw, sessionRaw}
	if len(broker.calls) != len(wantFields) {
		t.Fatalf("broker calls = %#v, want %d calls", broker.calls, len(wantFields))
	}
	for index, field := range wantFields {
		wantScope := scope
		wantScope.Field = field
		if broker.calls[index].scope != wantScope || broker.calls[index].raw != wantRaw[index] {
			t.Fatalf(
				"broker call[%d] = %#v, want scope=%#v raw=%q",
				index, broker.calls[index], wantScope, wantRaw[index],
			)
		}
	}
	if got := p.config.Auth.Header["Authorization"]; got != aiProxySecretDescriptor(values[headerRaw]) {
		t.Fatalf("public header credential = %q", got)
	}
	if got := p.config.Auth.Query["api-key"]; got != aiProxySecretDescriptor(values[queryRaw]) {
		t.Fatalf("public query credential = %q", got)
	}
	if got := p.config.Auth.GCP.ServiceAccountJSON; got != aiProxySecretDescriptor(values[gcpRaw]) {
		t.Fatalf("public GCP credential = %q", got)
	}
	if got := p.config.Auth.AWS.SecretAccessKey; got != aiProxySecretDescriptor(values[awsRaw]) {
		t.Fatalf("public AWS secret = %q", got)
	}
	if got := p.config.Auth.AWS.SessionToken; got != aiProxySecretDescriptor(values[sessionRaw]) {
		t.Fatalf("public AWS session = %q", got)
	}

	if err := p.withAuth(func(auth Auth) error {
		if auth.Header["Authorization"] != values[headerRaw] || auth.Query["api-key"] != values[queryRaw] {
			t.Fatalf("request auth maps = %#v/%#v", auth.Header, auth.Query)
		}
		if auth.GCP == nil || auth.GCP.ServiceAccountJSON != values[gcpRaw] || auth.GCP.MaxTTL != 30 {
			t.Fatalf("request GCP auth = %#v", auth.GCP)
		}
		if auth.AWS == nil || auth.AWS.AccessKeyID != "public-access-key" ||
			auth.AWS.SecretAccessKey != values[awsRaw] || auth.AWS.SessionToken != values[sessionRaw] {
			t.Fatalf("request AWS auth = %#v", auth.AWS)
		}
		return nil
	}); err != nil {
		t.Fatalf("withAuth() error = %v", err)
	}
}

func TestAIProxyStopDrainsCredentialUseAndClosesIdleConnections(t *testing.T) {
	const raw = "$ENV://AI_PROXY_STOP_HEADER"
	secrets, scope, _, closeAttempt := newAIProxyScopedSecretHarness(
		t, name, map[string]string{raw: "Bearer private-stop-token"},
	)
	defer closeAttempt()

	p := &Plugin{config: Config{
		Provider: "openai-compatible",
		Auth: Auth{Header: map[string]string{
			"Authorization": raw,
		}},
		Override: Override{Endpoint: "http://provider.test/v1/chat/completions"},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	transport := &aiProxyCloseRecordingTransport{closed: make(chan struct{})}
	p.client.Transport = transport

	entered := make(chan struct{})
	release := make(chan struct{})
	useDone := make(chan error, 1)
	go func() {
		useDone <- p.withAuth(func(auth Auth) error {
			if got := auth.Header["Authorization"]; got != "Bearer private-stop-token" {
				return fmt.Errorf("Authorization = %q", got)
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	<-transport.closed
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the active credential use drained")
	default:
	}
	close(release)
	if err := <-useDone; err != nil {
		t.Fatalf("active withAuth() error = %v", err)
	}
	<-stopDone

	if got := transport.closes.Load(); got != 1 {
		t.Fatalf("CloseIdleConnections() calls = %d, want 1", got)
	}
	if err := p.withAuth(func(Auth) error { return nil }); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop withAuth() error = %v, want credential unavailable", err)
	}
	if p.secrets.prepared || p.secrets.headers != nil || p.secrets.queries != nil ||
		p.secrets.gcp != (secret.Value{}) || p.secrets.awsSecret != (secret.Value{}) ||
		p.secrets.awsSession != (secret.Value{}) {
		t.Fatal("credential state survived Stop")
	}
	p.Stop()
	if got := transport.closes.Load(); got != 1 {
		t.Fatalf("CloseIdleConnections() calls after repeated Stop = %d, want 1", got)
	}
}
