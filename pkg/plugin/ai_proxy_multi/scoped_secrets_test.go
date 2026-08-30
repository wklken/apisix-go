package ai_proxy_multi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type aiProxyMultiScopedSecretCall struct {
	scope secret.Scope
	raw   string
}

type aiProxyMultiScopedSecretBroker struct {
	values map[string]string
	calls  []aiProxyMultiScopedSecretCall
}

func (*aiProxyMultiScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*aiProxyMultiScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this plugin fixture")
}

func (broker *aiProxyMultiScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.calls = append(broker.calls, aiProxyMultiScopedSecretCall{scope: scope, raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing scoped AI proxy multi credential")
	}
	return value, nil
}

func (*aiProxyMultiScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func newAIProxyMultiScopedSecretHarness(
	t *testing.T, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *aiProxyMultiScopedSecretBroker, func()) {
	t.Helper()
	const revision = uint64(122)
	key := generation.ResourceKey{Kind: "routes", ID: "ai-proxy-multi-scoped"}
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
			Key: key, Disposition: generation.DispositionPublished, Code: "ai-proxy-multi-scoped-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
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
	broker := &aiProxyMultiScopedSecretBroker{values: values}
	registration, err := testutil.NewSecretMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		_ = registration.Close(context.Background())
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision, Attempt: registration.AttemptID(), Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close scoped AI proxy multi attempt: %v", err)
		}
	}
}

func aiProxyMultiSecretDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func TestMaterializeScopedSecretsOwnsEveryAIProxyMultiInstance(t *testing.T) {
	const (
		firstHeader = "$ENV://AI_MULTI_FIRST_HEADER"
		firstQuery  = "$secret://ai-multi/first-query"
		secondGCP   = "$secret://ai-multi/gcp"
		secondAWS   = "$ENV://AI_MULTI_AWS_SECRET"
		secondToken = "$ENV://AI_MULTI_AWS_SESSION"
	)
	values := map[string]string{
		firstHeader: "Bearer first", firstQuery: "first-query",
		secondGCP: `{"client_email":"multi@example.com"}`,
		secondAWS: "multi-aws-secret", secondToken: "multi-session",
	}
	capabilityValue, scope, broker, closeAttempt := newAIProxyMultiScopedSecretHarness(t, values)
	defer closeAttempt()
	p := &Plugin{config: Config{Instances: []Instance{
		{Auth: Auth{
			Header: map[string]string{"Authorization": firstHeader},
			Query:  map[string]string{"key": firstQuery},
		}},
		{Auth: Auth{
			GCP: &ai_auth.GCPConfig{ServiceAccountJSON: secondGCP, MaxTTL: 45},
			AWS: &ai_auth.AWSConfig{
				AccessKeyID: "multi-access", SecretAccessKey: secondAWS, SessionToken: secondToken,
			},
		}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	wantFields := []string{
		"instances.*.auth.header", "instances.*.auth.query",
		"instances.*.auth.gcp.service_account_json",
		"instances.*.auth.aws.secret_access_key", "instances.*.auth.aws.session_token",
	}
	wantRaw := []string{firstHeader, firstQuery, secondGCP, secondAWS, secondToken}
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
	wantHeaderDescriptor := aiProxyMultiSecretDescriptor(values[firstHeader])
	if got := p.config.Instances[0].Auth.Header["Authorization"]; got != wantHeaderDescriptor {
		t.Fatalf("public first header = %q", got)
	}
	wantGCPDescriptor := aiProxyMultiSecretDescriptor(values[secondGCP])
	if got := p.config.Instances[1].Auth.GCP.ServiceAccountJSON; got != wantGCPDescriptor {
		t.Fatalf("public second GCP credential = %q", got)
	}
	wantAWSDescriptor := aiProxyMultiSecretDescriptor(values[secondAWS])
	if got := p.config.Instances[1].Auth.AWS.SecretAccessKey; got != wantAWSDescriptor {
		t.Fatalf("public second AWS secret = %q", got)
	}

	if err := p.withInstanceAuth(0, func(auth Auth) error {
		if auth.Header["Authorization"] != values[firstHeader] || auth.Query["key"] != values[firstQuery] {
			t.Fatalf("first request auth = %#v", auth)
		}
		return nil
	}); err != nil {
		t.Fatalf("withInstanceAuth(0) error = %v", err)
	}
	if err := p.withInstanceAuth(1, func(auth Auth) error {
		if auth.GCP == nil || auth.GCP.ServiceAccountJSON != values[secondGCP] || auth.GCP.MaxTTL != 45 {
			t.Fatalf("second request GCP auth = %#v", auth.GCP)
		}
		if auth.AWS == nil || auth.AWS.AccessKeyID != "multi-access" ||
			auth.AWS.SecretAccessKey != values[secondAWS] || auth.AWS.SessionToken != values[secondToken] {
			t.Fatalf("second request AWS auth = %#v", auth.AWS)
		}
		return nil
	}); err != nil {
		t.Fatalf("withInstanceAuth(1) error = %v", err)
	}
}
