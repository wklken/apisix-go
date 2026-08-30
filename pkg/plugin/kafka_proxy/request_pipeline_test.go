package kafka_proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type pipelineScopedSecretBroker struct{}

func (pipelineScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (pipelineScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (pipelineScopedSecretBroker) ResolveScoped(
	ctx context.Context, _ secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if raw != "$ENV://KAFKA_PIPELINE_PASSWORD" {
		return "", errors.New("unexpected raw value")
	}
	return "pipeline-password", nil
}

func (pipelineScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func newPipelineKafkaProxy(t *testing.T, revision uint64) (*kafka_proxy.Plugin, func()) {
	t.Helper()
	p := &kafka_proxy.Plugin{}
	config := p.Config().(*kafka_proxy.Config)
	config.SASL = &kafka_proxy.SASL{
		Username: "user", Password: "$ENV://KAFKA_PIPELINE_PASSWORD",
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	key := generation.ResourceKey{Kind: "routes", ID: "kafka-pipeline"}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{"kafka-proxy":{}}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {
				Artifact: generation.GenerationArtifact{
					Domain: generation.DomainHTTP, Revision: revision,
					Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
				},
				Snapshot: snapshot,
				Closure:  []generation.ResourceKey{key},
				Decisions: []generation.ResourceDecision{{
					Key: key, Disposition: generation.DispositionPublished, Code: "kafka-pipeline-test",
				}},
			},
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
	registration, err := testutil.NewSecretMaterializer(pipelineScopedSecretBroker{}, catalog).
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
		Plugin:     "kafka-proxy",
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func TestRequestPipelineKeepsKafkaSASLCallbackAcrossTerminalAndStop(t *testing.T) {
	p, closeAttempt := newPipelineKafkaProxy(t, 51)
	defer closeAttempt()
	descriptor, err := plugin.ResolveDescriptorForFactory("kafka-proxy", p)
	if err != nil {
		t.Fatalf("ResolveDescriptorForFactory() error = %v", err)
	}
	if _, ok := any(p).(base.RequestPhasePlugin); ok {
		t.Fatal("*kafka_proxy.Plugin implements RequestPhasePlugin; RequestPipeline would bypass Handler")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	requestDone := make(chan struct{})
	stopDone := make(chan struct{})
	var (
		calls    atomic.Int32
		retained *http.Request
	)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		retained = r
		if got := kafka_proxy.SASLPassword(r); got != "pipeline-password" {
			t.Errorf("SASLPassword() in terminal = %q, want pipeline-password", got)
		}
		close(entered)
		<-release
		if got := kafka_proxy.SASLPassword(r); got != "pipeline-password" {
			t.Errorf("SASLPassword() before terminal return = %q, want pipeline-password", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := plugin.NewRequestPipeline([]plugin.Binding{{
		Plugin: p, Descriptor: descriptor, Scope: plugin.ScopeRoute,
	}}, nil).Then(terminal)
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/kafka", nil),
		time.Now(),
	)
	response := httptest.NewRecorder()
	go func() {
		handler.ServeHTTP(response, request)
		close(requestDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RequestPipeline terminal")
	}

	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned while the RequestPipeline terminal held the SASL callback")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RequestPipeline request completion")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Stop after terminal return")
	}
	if calls.Load() != 1 || response.Code != http.StatusNoContent {
		t.Fatalf("terminal calls/status = %d/%d, want 1/204", calls.Load(), response.Code)
	}
	if retained == nil {
		t.Fatal("RequestPipeline terminal did not retain its request fixture")
	}
	if got := kafka_proxy.SASLPassword(retained); got != "" {
		t.Fatalf("retained RequestPipeline password after callback = %q, want cleared", got)
	}
}

func TestRequestPipelineWithoutLifecycleCallsKafkaTerminalOnce(t *testing.T) {
	p, closeAttempt := newPipelineKafkaProxy(t, 52)
	defer closeAttempt()
	descriptor, err := plugin.ResolveDescriptorForFactory("kafka-proxy", p)
	if err != nil {
		t.Fatalf("ResolveDescriptorForFactory() error = %v", err)
	}
	var calls int
	response := httptest.NewRecorder()
	plugin.NewRequestPipeline([]plugin.Binding{{
		Plugin: p, Descriptor: descriptor, Scope: plugin.ScopeRoute,
	}}, nil).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := kafka_proxy.SASLPassword(r); got != "pipeline-password" {
			t.Fatalf("SASLPassword() = %q, want pipeline-password", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/kafka", nil))
	if calls != 1 || response.Code != http.StatusNoContent {
		t.Fatalf("terminal calls/status = %d/%d, want 1/204", calls, response.Code)
	}
}
