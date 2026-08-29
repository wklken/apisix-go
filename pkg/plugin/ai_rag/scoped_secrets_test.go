package ai_rag

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	failure := broker.fail[raw]
	value, found := broker.values[raw]
	hook := broker.hook
	broker.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if failure != nil {
		return "", failure
	}
	if found {
		return value, nil
	}
	return raw, nil
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

func (broker *scopedSecretBroker) callsSnapshot() []scopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]scopedSecretCall(nil), broker.calls...)
}

func (broker *scopedSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
		return
	}
	broker.fail[raw] = err
}

func (broker *scopedSecretBroker) resetCalls() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = nil
}

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

func ragConfig(embeddingEndpoint, embeddingKey, searchEndpoint, searchKey string) Config {
	return Config{
		EmbeddingsProvider: EmbeddingsProvider{AzureOpenAI: AzureProvider{
			Endpoint: embeddingEndpoint,
			APIKey:   embeddingKey,
		}},
		VectorSearchProvider: VectorSearchProvider{AzureAISearch: AzureProvider{
			Endpoint: searchEndpoint,
			APIKey:   searchKey,
		}},
	}
}

func wantRAGDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func assertRAGScopedCalls(
	t *testing.T, baseScope secret.Scope, calls []scopedSecretCall, raws []string,
) {
	t.Helper()
	fields := []string{
		"embeddings_provider.azure_openai.api_key",
		"vector_search_provider.azure_ai_search.api_key",
	}
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

func runRAGRequest(p *Plugin) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "messages":[],
	  "ai_rag":{"embeddings":{"input":"hello"},"vector_search":{"fields":"contentVector"}}
	}`))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	return response
}

func TestScopedSecretsMaterializeRAGProviderKeys(t *testing.T) {
	const (
		embeddingRaw = "$ENV://RAG_SCOPED_EMBEDDING_KEY"
		searchRaw    = "$ENV://RAG_SCOPED_SEARCH_KEY"
		embeddingKey = "resolved-embedding-key"
		searchKey    = "resolved-search-key"
	)
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != embeddingKey {
			t.Errorf("embedding api-key = %q, want resolved credential", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
	}))
	t.Cleanup(embeddings.Close)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("api-key"); got != searchKey {
			t.Errorf("search api-key = %q, want resolved credential", got)
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	t.Cleanup(search.Close)

	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		embeddingRaw: embeddingKey,
		searchRaw:    searchKey,
	})
	defer closeAttempt()
	p := &Plugin{config: ragConfig(embeddings.URL, embeddingRaw, search.URL, searchRaw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{embeddingRaw, searchRaw})
	if got := p.config.EmbeddingsProvider.AzureOpenAI.APIKey; got != wantRAGDescriptor(embeddingKey) {
		t.Fatalf("embedding descriptor = %q, want resolved plaintext digest", got)
	}
	if got := p.config.VectorSearchProvider.AzureAISearch.APIKey; got != wantRAGDescriptor(searchKey) {
		t.Fatalf("search descriptor = %q, want resolved plaintext digest", got)
	}
	for _, sensitive := range []string{
		embeddingRaw, searchRaw, "RAG_SCOPED_EMBEDDING_KEY", "RAG_SCOPED_SEARCH_KEY",
		embeddingKey, searchKey,
	} {
		if strings.Contains(fmt.Sprintf("%#v", p.config), sensitive) {
			t.Fatalf("effective config retained %q: %#v", sensitive, p.config)
		}
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	response := runRAGRequest(p)
	if response.Code != http.StatusNoContent {
		t.Fatalf("RAG response = %d %q, want 204", response.Code, response.Body.String())
	}
}

func TestScopedSecretsResolveManagedRAGSearchKey(t *testing.T) {
	const managed = "$secret://vault/ai-rag/search-key"
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		"literal-embedding": "literal-embedding",
		managed:             "managed-search",
	})
	defer closeAttempt()
	p := &Plugin{config: ragConfig(
		"http://127.0.0.1", "literal-embedding", "http://127.0.0.1", managed,
	)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{"literal-embedding", managed})
	if p.config.VectorSearchProvider.AzureAISearch.APIKey != wantRAGDescriptor("managed-search") {
		t.Fatalf(
			"managed search config = %q, want resolved descriptor",
			p.config.VectorSearchProvider.AzureAISearch.APIKey,
		)
	}
	if strings.Contains(fmt.Sprintf("%#v", p.config), managed) ||
		strings.Contains(fmt.Sprintf("%#v", p.config), "managed-search") {
		t.Fatalf("managed search material leaked through config: %#v", p.config)
	}
}

func TestScopedSecretsRAGSecondKeyFailureIsAtomic(t *testing.T) {
	const (
		embeddingRaw = "$ENV://RAG_RETRY_EMBEDDING_KEY"
		searchRaw    = "$secret://vault/ai-rag/retry-search-key"
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		embeddingRaw: "retry-embedding",
		searchRaw:    "retry-search",
	})
	defer closeAttempt()
	broker.setFailure(searchRaw, errors.New("broker failure contains "+searchRaw+" retry-search"))
	p := &Plugin{config: ragConfig("http://127.0.0.1", embeddingRaw, "http://127.0.0.1", searchRaw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil, want second-key failure")
	}
	for _, sensitive := range []string{embeddingRaw, searchRaw, "RAG_RETRY_EMBEDDING_KEY", "retry-embedding", "retry-search", "broker failure"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("materialization error = %v, contains %q", err, sensitive)
		}
	}
	if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != embeddingRaw ||
		p.config.VectorSearchProvider.AzureAISearch.APIKey != searchRaw {
		t.Fatalf("failed materialization changed public config: %#v", p.config)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() succeeded after atomic materialization failure")
	}

	broker.setFailure(searchRaw, nil)
	broker.resetCalls()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{embeddingRaw, searchRaw})
	if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != wantRAGDescriptor("retry-embedding") ||
		p.config.VectorSearchProvider.AzureAISearch.APIKey != wantRAGDescriptor("retry-search") {
		t.Fatalf("retry did not install resolved descriptors: %#v", p.config)
	}
}

func TestPostInitDoesNotSelfMaterializeRAGKeys(t *testing.T) {
	const (
		embeddingEnv = "RAG_POST_INIT_EMBEDDING"
		searchEnv    = "RAG_POST_INIT_SEARCH"
	)
	t.Setenv(embeddingEnv, "post-init-embedding")
	t.Setenv(searchEnv, "post-init-search")
	p := &Plugin{config: ragConfig(
		"http://127.0.0.1", "$ENV://"+embeddingEnv,
		"http://127.0.0.1", "$ENV://"+searchEnv,
	)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := p.PostInit()
	if err == nil || err.Error() != "ai-rag provider credentials are unavailable" {
		t.Fatalf("PostInit() error = %v, want redacted credential-unavailable error", err)
	}
	if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != "$ENV://"+embeddingEnv ||
		p.config.VectorSearchProvider.AzureAISearch.APIKey != "$ENV://"+searchEnv {
		t.Fatalf("PostInit() changed raw config: %#v", p.config)
	}
}

func TestScopedSecretsRAGProviderModesUseResolvedPlaintextDescriptors(t *testing.T) {
	encryption := testutil.DataEncryptionService(true, []string{"0123456789abcdef"})
	contextual, err := encryption.EncryptForContext(
		"contextual-embedding",
		name+".embeddings_provider.azure_openai.api_key",
	)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	for _, test := range []struct {
		name      string
		raw       string
		plaintext string
	}{
		{name: "literal", raw: "literal-embedding", plaintext: "literal-embedding"},
		{name: "contextual ciphertext", raw: contextual, plaintext: "contextual-embedding"},
		{name: "environment", raw: "$ENV://RAG_MODE_EMBEDDING", plaintext: "environment-embedding"},
		{name: "managed", raw: "$secret://vault/ai-rag/mode-embedding", plaintext: "managed-embedding"},
	} {
		t.Run(test.name, func(t *testing.T) {
			const searchRaw = "literal-search"
			capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
				test.raw:  test.plaintext,
				searchRaw: searchRaw,
			})
			defer closeAttempt()
			p := &Plugin{config: ragConfig(
				"http://127.0.0.1", test.raw, "http://127.0.0.1", searchRaw,
			)}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{test.raw, searchRaw})
			if got := p.config.EmbeddingsProvider.AzureOpenAI.APIKey; got != wantRAGDescriptor(test.plaintext) {
				t.Fatalf("embedding descriptor = %q, want complete resolved plaintext digest", got)
			}
			for _, sensitive := range []string{test.raw, test.plaintext} {
				if sensitive != "" && strings.Contains(fmt.Sprintf("%#v", p.config), sensitive) {
					t.Fatalf("effective config retained %q: %#v", sensitive, p.config)
				}
			}
		})
	}
}

func TestScopedSecretsRAGRejectResolvedBlankKeysAndRetry(t *testing.T) {
	const (
		embeddingRaw = "$ENV://RAG_BLANK_EMBEDDING"
		searchRaw    = "$secret://vault/ai-rag/blank-search"
	)
	for _, test := range []struct {
		name       string
		invalidRaw string
		blank      string
		wantCalls  int
	}{
		{name: "embedding empty", invalidRaw: embeddingRaw, blank: "", wantCalls: 1},
		{name: "embedding whitespace", invalidRaw: embeddingRaw, blank: " \t\n", wantCalls: 1},
		{name: "search empty", invalidRaw: searchRaw, blank: "", wantCalls: 2},
		{name: "search whitespace", invalidRaw: searchRaw, blank: " \t\n", wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				embeddingRaw: "valid-embedding",
				searchRaw:    "valid-search",
			}
			values[test.invalidRaw] = test.blank
			capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, values)
			defer closeAttempt()
			p := &Plugin{config: ragConfig(
				"http://127.0.0.1", embeddingRaw, "http://127.0.0.1", searchRaw,
			)}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}

			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
				t.Fatalf("blank resolved key error = %v, want redacted credential unavailable", err)
			}
			if got := len(broker.callsSnapshot()); got != test.wantCalls {
				t.Fatalf("broker calls = %d, want %d before blank rejection", got, test.wantCalls)
			}
			if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != embeddingRaw ||
				p.config.VectorSearchProvider.AzureAISearch.APIKey != searchRaw {
				t.Fatalf("blank resolved key installed config: %#v", p.config)
			}

			broker.setValue(test.invalidRaw, "retry-valid")
			broker.resetCalls()
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("same-instance retry error = %v", err)
			}
			assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{embeddingRaw, searchRaw})
			wantEmbedding := "valid-embedding"
			wantSearch := "valid-search"
			if test.invalidRaw == embeddingRaw {
				wantEmbedding = "retry-valid"
			} else {
				wantSearch = "retry-valid"
			}
			if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != wantRAGDescriptor(wantEmbedding) ||
				p.config.VectorSearchProvider.AzureAISearch.APIKey != wantRAGDescriptor(wantSearch) {
				t.Fatalf("retry descriptors = %#v, want resolved-value digests", p.config)
			}
		})
	}
}

func TestScopedSecretsRAGConcurrentMaterializationIsSingleFlight(t *testing.T) {
	const (
		embeddingRaw = "$ENV://RAG_SINGLEFLIGHT_EMBEDDING"
		searchRaw    = "$ENV://RAG_SINGLEFLIGHT_SEARCH"
		workers      = 32
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		embeddingRaw: "singleflight-embedding",
		searchRaw:    "singleflight-search",
	})
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call scopedSecretCall) {
		if call.Scope.Field == "embeddings_provider.azure_openai.api_key" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: ragConfig(
		"http://127.0.0.1", embeddingRaw, "http://127.0.0.1", searchRaw,
	)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		go func() {
			ready.Done()
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped materialization leader")
	}
	close(release)
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent scoped materialization error = %v", err)
		}
	}
	assertRAGScopedCalls(t, scope, broker.callsSnapshot(), []string{embeddingRaw, searchRaw})
}

func TestScopedSecretsRAGStopDuringMaterializeCannotReviveState(t *testing.T) {
	const (
		embeddingRaw = "$ENV://RAG_STOP_RACE_EMBEDDING"
		searchRaw    = "$ENV://RAG_STOP_RACE_SEARCH"
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		embeddingRaw: "stop-race-embedding",
		searchRaw:    "stop-race-search",
	})
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call scopedSecretCall) {
		if call.Scope.Field == "embeddings_provider.azure_openai.api_key" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: ragConfig(
		"http://127.0.0.1", embeddingRaw, "http://127.0.0.1", searchRaw,
	)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeDone := make(chan error, 1)
	go func() {
		materializeDone <- base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped resolution")
	}
	p.Stop()
	close(release)
	if err := <-materializeDone; err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("materialization racing Stop() error = %v, want redacted terminal failure", err)
	}
	if p.config.EmbeddingsProvider.AzureOpenAI.APIKey != embeddingRaw ||
		p.config.VectorSearchProvider.AzureAISearch.APIKey != searchRaw ||
		p.scopedSet {
		t.Fatalf("materialization revived retired state: config=%#v", p.config)
	}
	callCount := len(broker.callsSnapshot())
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err == nil {
		t.Fatal("scoped materialization after Stop() error = nil")
	}
	if got := len(broker.callsSnapshot()); got != callCount {
		t.Fatalf("broker calls after terminal Stop() = %d, want %d", got, callCount)
	}
}

func prepareScopedRAGPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	embeddingEndpoint string,
	searchEndpoint string,
	embeddingKey string,
	searchKey string,
) (*Plugin, func()) {
	t.Helper()
	embeddingRaw := "$ENV://RAG_" + resourceID + "_EMBEDDING"
	searchRaw := "$secret://vault/ai-rag/" + resourceID + "-search"
	capabilityValue, scope, _, closeAttempt := newScopedSecretHarnessAt(
		t, name, revision, resourceID, map[string]string{
			embeddingRaw: embeddingKey,
			searchRaw:    searchKey,
		},
	)
	p := &Plugin{config: ragConfig(embeddingEndpoint, embeddingRaw, searchEndpoint, searchRaw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p, closeAttempt
}

func TestScopedSecretsRAGGenerationInstancesDoNotCrossUseCredentials(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generationID := r.URL.Query().Get("generation")
		want := generationID + "-embedding"
		if strings.Contains(r.URL.Path, "search") {
			want = generationID + "-search"
		}
		if got := r.Header.Get("api-key"); got != want {
			t.Errorf("%s api-key = %q, want %q", r.URL.Path, got, want)
		}
		if strings.Contains(r.URL.Path, "embedding") {
			_, _ = w.Write([]byte(`{"data":[{"embedding":[1]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":[]}`))
	}))
	t.Cleanup(provider.Close)

	pN, closeN := prepareScopedRAGPlugin(
		t, 11, "generation-n",
		provider.URL+"/embedding?generation=generation-n",
		provider.URL+"/search?generation=generation-n",
		"generation-n-embedding", "generation-n-search",
	)
	pN1, closeN1 := prepareScopedRAGPlugin(
		t, 12, "generation-n1",
		provider.URL+"/embedding?generation=generation-n1",
		provider.URL+"/search?generation=generation-n1",
		"generation-n1-embedding", "generation-n1-search",
	)
	defer closeN()
	defer closeN1()
	t.Cleanup(pN.Stop)
	t.Cleanup(pN1.Stop)

	var wg sync.WaitGroup
	for _, plugin := range []*Plugin{pN, pN1} {
		wg.Add(1)
		go func(plugin *Plugin) {
			defer wg.Done()
			response := runRAGRequest(plugin)
			if response.Code != http.StatusNoContent {
				t.Errorf("generation request = %d %q, want 204", response.Code, response.Body.String())
			}
		}(plugin)
	}
	wg.Wait()
	pN.Stop()
	if response := runRAGRequest(pN1); response.Code != http.StatusNoContent {
		t.Fatalf("N+1 after N retirement = %d %q, want 204", response.Code, response.Body.String())
	}
	if response := runRAGRequest(pN); response.Code != http.StatusInternalServerError {
		t.Fatalf("retired N request = %d %q, want credential failure", response.Code, response.Body.String())
	}
}

type retainingRAGTransport struct {
	mu            sync.Mutex
	requests      []*http.Request
	apiKeyHeaders [][]string
}

func (transport *retainingRAGTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.requests = append(transport.requests, request)
	transport.apiKeyHeaders = append(
		transport.apiKeyHeaders,
		request.Header[http.CanonicalHeaderKey("api-key")],
	)
	transport.mu.Unlock()
	body := `{"value":[]}`
	if strings.Contains(request.URL.Path, "embedding") {
		body = `{"data":[{"embedding":[1]}]}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

func TestRAGProviderRequestsClearDerivedAPIKeyHeaders(t *testing.T) {
	p, closeAttempt := prepareScopedRAGPlugin(
		t, 20, "header-cleanup",
		"http://provider.test/embedding", "http://provider.test/search",
		"cleanup-embedding", "cleanup-search",
	)
	defer closeAttempt()
	t.Cleanup(p.Stop)
	transport := &retainingRAGTransport{}
	p.client = &http.Client{Transport: transport}

	response := runRAGRequest(p)
	if response.Code != http.StatusNoContent {
		t.Fatalf("RAG response = %d %q, want 204", response.Code, response.Body.String())
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.requests) != 2 || len(transport.apiKeyHeaders) != 2 {
		t.Fatalf("retained provider requests = %d, want 2", len(transport.requests))
	}
	for index, request := range transport.requests {
		if values, present := request.Header[http.CanonicalHeaderKey("api-key")]; present || len(values) != 0 {
			t.Fatalf("retained request[%d] api-key = %#v, want removed", index, values)
		}
		for valueIndex, value := range transport.apiKeyHeaders[index] {
			if value != "" {
				t.Fatalf("retained api-key slice[%d][%d] = %q, want cleared", index, valueIndex, value)
			}
		}
	}
	clientIdentity := fmt.Sprintf("%#v", p.client)
	for _, credential := range []string{"cleanup-embedding", "cleanup-search"} {
		if strings.Contains(clientIdentity, credential) {
			t.Fatalf("retained provider client identity contains %q: %s", credential, clientIdentity)
		}
	}
}

type blockingRAGTransport struct {
	entered  chan *http.Request
	release  chan struct{}
	requests atomic.Int32
	closed   atomic.Bool
}

func (transport *blockingRAGTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requests.Add(1)
	transport.entered <- request
	<-transport.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[1]}]}`)),
		Request:    request,
	}, nil
}

func (transport *blockingRAGTransport) CloseIdleConnections() {
	transport.closed.Store(true)
}

func waitForRAGRetirement(t *testing.T, p *Plugin, transport *blockingRAGTransport) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.credentialMu.Lock()
		retired := p.retired
		activeUses := p.activeUses
		p.credentialMu.Unlock()
		if retired && activeUses == 1 && transport.closed.Load() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for retired client with one active credential use")
}

func TestRAGStopDrainsProviderUseAndRetiresScopedCredentials(t *testing.T) {
	for _, mode := range []string{"scoped"} {
		t.Run(mode, func(t *testing.T) {
			var (
				p            *Plugin
				closeAttempt func()
			)
			p, closeAttempt = prepareScopedRAGPlugin(
				t, 30, "stop-scoped",
				"http://provider.test/embedding", "http://provider.test/search",
				"stop-scoped-embedding", "stop-scoped-search",
			)
			defer closeAttempt()

			transport := &blockingRAGTransport{
				entered: make(chan *http.Request, 1),
				release: make(chan struct{}),
			}
			p.client = &http.Client{Transport: transport}
			requestDone := make(chan error, 1)
			go func() {
				_, status, message := p.requestEmbeddings(
					httptest.NewRequest(http.MethodPost, "/", nil),
					map[string]any{"input": "hello"},
				)
				if status != http.StatusOK {
					requestDone <- fmt.Errorf("request status = %d: %s", status, message)
					return
				}
				requestDone <- nil
			}()
			var retainedRequest *http.Request
			select {
			case retainedRequest = <-transport.entered:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for provider request")
			}
			retainedHeader := retainedRequest.Header[http.CanonicalHeaderKey("api-key")]
			firstStop := make(chan struct{})
			secondStop := make(chan struct{})
			go func() { p.Stop(); close(firstStop) }()
			go func() { p.Stop(); close(secondStop) }()
			waitForRAGRetirement(t, p, transport)
			for _, stopDone := range []chan struct{}{firstStop, secondStop} {
				select {
				case <-stopDone:
					t.Fatal("Stop() returned before in-flight provider use completed")
				default:
				}
			}
			if _, status, message := p.requestEmbeddings(
				httptest.NewRequest(http.MethodPost, "/", nil), map[string]any{"input": "new"},
			); status != http.StatusInternalServerError || message != errRAGCredentialsUnavailable.Error() {
				t.Fatalf("request after retirement = (%d, %q), want credential unavailable", status, message)
			}
			if got := transport.requests.Load(); got != 1 {
				t.Fatalf("provider requests after retirement = %d, want 1", got)
			}

			close(transport.release)
			if err := <-requestDone; err != nil {
				t.Fatal(err)
			}
			<-firstStop
			<-secondStop
			p.Stop()
			for index, value := range retainedHeader {
				if value != "" {
					t.Fatalf("retained request header[%d] = %q, want cleared", index, value)
				}
			}
			if _, present := retainedRequest.Header[http.CanonicalHeaderKey("api-key")]; present {
				t.Fatal("retained provider request still has api-key header")
			}
			p.credentialMu.Lock()
			clientSet := p.client != nil
			scopedEmbeddingSet := p.scopedEmbeddingAPIKey != (secret.Value{})
			scopedSearchSet := p.scopedSearchAPIKey != (secret.Value{})
			scopedSet := p.scopedSet
			retired := p.retired
			activeUses := p.activeUses
			p.credentialMu.Unlock()
			if clientSet || scopedEmbeddingSet || scopedSearchSet || scopedSet || !retired || activeUses != 0 {
				t.Fatalf(
					"retained RAG state after Stop(): client=%v scoped_values=(%v,%v) "+
						"scoped=%v retired=%v active_uses=%d",
					clientSet, scopedEmbeddingSet, scopedSearchSet, scopedSet, retired, activeUses,
				)
			}
		})
	}
}
