package secret

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
)

func TestGenerationSecretResolverImplementsAttemptFactory(t *testing.T) {
	var factory AttemptResolverFactory = newGenerationSecretResolverForTest(t)
	if _, ok := factory.(*GenerationSecretResolver); !ok {
		t.Fatalf("factory type = %T, want *GenerationSecretResolver", factory)
	}
}

func TestGenerationSecretResolverRejectsUnconfiguredEncryption(t *testing.T) {
	if _, err := NewGenerationSecretResolver(data_encryption.Service{}); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("constructor error = %v, want ErrInvalidCapability", err)
	}
}

func newGenerationSecretResolverForTest(t *testing.T) *GenerationSecretResolver {
	t.Helper()
	service, _ := testService(t, false)
	resolver, err := NewGenerationSecretResolver(service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close(context.Background()) })
	return resolver
}

type generationResolverFixture struct {
	ticket    generation.ApplyTicket
	set       generation.PublicationSet
	scope     Scope
	routeKey  generation.ResourceKey
	secretKey generation.ResourceKey
}

func newGenerationResolverFixture(
	t *testing.T,
	domain generation.Domain,
	revision uint64,
	routeValue []byte,
	secretConfig []byte,
) generationResolverFixture {
	t.Helper()
	routeKey := generation.ResourceKey{Kind: "routes", ID: "route-1"}
	resources := []generation.Resource{{Key: routeKey, Value: append([]byte(nil), routeValue...)}}
	closure := []generation.ResourceKey{routeKey}
	decisions := []generation.ResourceDecision{{
		Key: routeKey, Disposition: generation.DispositionPublished, Code: "ok",
	}}
	secretKey := generation.ResourceKey{}
	if secretConfig != nil {
		secretKey = generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}
		resources = append(resources, generation.Resource{Key: secretKey, Value: append([]byte(nil), secretConfig...)})
		closure = append(closure, secretKey)
		decisions = append(decisions, generation.ResourceDecision{
			Key: secretKey, Disposition: generation.DispositionPublished, Code: "ok",
		})
	}
	snapshot, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: revision, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot:  snapshot,
		Closure:   closure,
		Decisions: decisions,
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   sha256.Sum256(routeValue),
		Cursor:          generation.ProviderCursor{Provider: "test", Revision: fmt.Sprintf("%d", revision)},
		RequiredDomains: []generation.Domain{domain},
	}
	return generationResolverFixture{
		ticket: ticket,
		set: generation.PublicationSet{
			DesiredRevision: revision,
			Domains:         map[generation.Domain]generation.PublicationCandidate{domain: candidate},
		},
		scope: Scope{
			Generation: revision,
			Domain:     domain,
			Plugin:     "key-auth",
			Resource:   routeKey,
			Source:     capability.SecretPluginConfig,
			Field:      "key",
		},
		routeKey:  routeKey,
		secretKey: secretKey,
	}
}

func resolverScope(fixture generationResolverFixture, id AttemptID) Scope {
	scope := fixture.scope
	scope.Attempt = id
	return scope
}

func vaultConfigBytesForResolver(t *testing.T, uri, token string) []byte {
	return vaultConfigBytesForResolverTimeout(t, uri, token, 2)
}

func vaultConfigBytesForResolverTimeout(t *testing.T, uri, token string, timeout int) []byte {
	t.Helper()
	encoded, err := json.Marshal(struct {
		URI       string `json:"uri"`
		Prefix    string `json:"prefix"`
		Token     string `json:"token"`
		Namespace string `json:"namespace,omitempty"`
		Timeout   int    `json:"timeout,omitempty"`
	}{URI: uri, Prefix: "kv/apisix", Token: token, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func allZeroResolverBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type resolverRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn resolverRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type resolverCloseTrackingTransport struct {
	base        *http.Transport
	closed      atomic.Bool
	closeCalls  atomic.Int32
	beforeClose func()
}

func (transport *resolverCloseTrackingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.base.RoundTrip(request)
}

func (transport *resolverCloseTrackingTransport) CloseIdleConnections() {
	transport.closeCalls.Add(1)
	if transport.beforeClose != nil {
		transport.beforeClose()
	}
	transport.closed.Store(true)
	transport.base.CloseIdleConnections()
}

type resolverPartialErrorReadCloser struct {
	payload []byte
	seen    []byte
	read    bool
}

func (reader *resolverPartialErrorReadCloser) Read(target []byte) (int, error) {
	if reader.read {
		return 0, io.ErrUnexpectedEOF
	}
	reader.read = true
	copy(target, reader.payload)
	reader.seen = target[:len(reader.payload)]
	return len(reader.payload), io.ErrUnexpectedEOF
}

func (reader *resolverPartialErrorReadCloser) Close() error { return nil }

var _ io.ReadCloser = (*resolverPartialErrorReadCloser)(nil)

func newGenerationSecretResolverWithTrackingTransport(
	t *testing.T,
) (*GenerationSecretResolver, *resolverCloseTrackingTransport) {
	t.Helper()
	service, _ := testService(t, false)
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not *http.Transport")
	}
	transport := &resolverCloseTrackingTransport{base: base.Clone()}
	resolver, err := newGenerationSecretResolver(service, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resolver.Close(context.Background()) })
	t.Cleanup(transport.base.CloseIdleConnections)
	return resolver, transport
}

func waitForGenerationAttemptDraining(t *testing.T, resolver *GenerationSecretResolver, id AttemptID) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		resolver.mu.Lock()
		_, live := resolver.attempts[id]
		_, draining := resolver.draining[id]
		resolver.mu.Unlock()
		if !live && draining {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("attempt did not enter draining state")
		case <-ticker.C:
		}
	}
}

func TestGenerationSecretResolverOpenCandidateRequiresExactIDAndClosure(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 9, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	wrong := id
	wrong[0]++
	if _, err := resolver.OpenCandidate(
		context.Background(),
		wrong,
		fixture.ticket,
		fixture.set,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("wrong ID error = %v, want ErrInvalidCapability", err)
	}
	missing := clonePublicationSet(fixture.set)
	delete(missing.Domains, generation.DomainHTTP)
	if _, err := resolver.OpenCandidate(
		context.Background(),
		id,
		fixture.ticket,
		missing,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("missing domain error = %v, want ErrInvalidCapability", err)
	}
	duplicate := clonePublicationSet(fixture.set)
	candidate := duplicate.Domains[generation.DomainHTTP]
	candidate.Closure = append(candidate.Closure, candidate.Closure[0])
	duplicate.Domains[generation.DomainHTTP] = candidate
	if _, err := resolver.OpenCandidate(
		context.Background(),
		id,
		fixture.ticket,
		duplicate,
	); !errors.Is(
		err,
		ErrInvalidCapability,
	) {
		t.Fatalf("duplicate closure error = %v, want ErrInvalidCapability", err)
	}
}

func TestGenerationSecretResolverClonesCandidateInputsAndIndexesOnlyPublishedClosure(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 9, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	attempt := opened.(*generationSecretAttempt)
	candidate := fixture.set.Domains[generation.DomainHTTP]
	clear(candidate.Closure)
	clear(candidate.Decisions)
	delete(fixture.set.Domains, generation.DomainHTTP)
	fixture.ticket.RequiredDomains[0] = generation.DomainStream
	got, err := attempt.ResolveScoped(context.Background(), resolverScope(fixture, id), "retained")
	if err != nil || got != "retained" {
		t.Fatalf("ResolveScoped() = %q/%v, want retained", got, err)
	}
	missing := resolverScope(fixture, id)
	missing.Resource = generation.ResourceKey{Kind: "routes", ID: "missing"}
	if _, err := attempt.ResolveScoped(
		context.Background(),
		missing,
		"retained",
	); !errors.Is(
		err,
		ErrCapabilityScopeMismatch,
	) {
		t.Fatalf("non-closure scope error = %v, want ErrCapabilityScopeMismatch", err)
	}
}

func TestGenerationSecretResolverRejectsDuplicateExactAttemptWhileDraining(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"resolved"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		9,
		[]byte("route"),
		vaultConfigBytesForResolver(t, server.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := opened.ResolveScoped(
			context.Background(),
			resolverScope(fixture, id),
			"$secret://vault/test1/foo/password",
		)
		resolveDone <- resolveErr
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- opened.Close(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resolver.mu.Lock()
		_, live := resolver.attempts[id]
		_, draining := resolver.draining[id]
		resolver.mu.Unlock()
		if !live && draining {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("attempt did not enter draining state")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := resolver.OpenCandidate(
		context.Background(),
		id,
		fixture.ticket,
		fixture.set,
	); !errors.Is(
		err,
		ErrAttemptAlreadyRegistered,
	) {
		close(release)
		t.Fatalf("duplicate open while draining error = %v, want ErrAttemptAlreadyRegistered", err)
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("in-flight ResolveScoped() error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set); err != nil {
		t.Fatalf("reopen after cleanup error = %v", err)
	}
}

func TestGenerationSecretResolverRejectsScopeAuthorityBeforeVault(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"must-not-read"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		11,
		[]byte("route"),
		vaultConfigBytesForResolver(t, server.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	base := resolverScope(fixture, id)
	mutations := map[string]func(*Scope){
		"attempt":    func(scope *Scope) { scope.Attempt[0]++ },
		"generation": func(scope *Scope) { scope.Generation++ },
		"domain":     func(scope *Scope) { scope.Domain = generation.DomainStream },
		"resource":   func(scope *Scope) { scope.Resource = generation.ResourceKey{Kind: "routes", ID: "other"} },
		"source":     func(scope *Scope) { scope.Source = capability.SecretPluginMetadata },
		"field":      func(scope *Scope) { scope.Field = "missing" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			scope := base
			mutate(&scope)
			if _, err := opened.ResolveScoped(
				context.Background(),
				scope,
				"$secret://vault/test1/foo/password",
			); err == nil {
				t.Fatal("ResolveScoped() error = nil for mismatched authority")
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests after rejected scopes = %d, want 0", got)
	}
}

func TestGenerationSecretResolverRequiresRetainedVaultResourceInSameDomain(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"must-not-read"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 13, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	_, err = opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	)
	if !errors.Is(err, ErrCapabilityScopeMismatch) {
		t.Fatalf("ResolveScoped() error = %v, want ErrCapabilityScopeMismatch", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests without retained resource = %d, want 0", got)
	}
}

func TestGenerationSecretResolverResolvesEnvironmentAndNestedEnvironmentReferences(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 15, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("APISIX_GO_GENERATION_ENV", "plain-value")
	plain, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$ENV://APISIX_GO_GENERATION_ENV",
	)
	if err != nil || plain != "plain-value" {
		t.Fatalf("plain environment value = %q/%v, want plain-value", plain, err)
	}
	t.Setenv("APISIX_GO_GENERATION_JSON", `{"credentials":{"password":"nested-value"}}`)
	nested, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$env://APISIX_GO_GENERATION_JSON/credentials/password",
	)
	if err != nil || nested != "nested-value" {
		t.Fatalf("nested environment value = %q/%v, want nested-value", nested, err)
	}
}

func TestGenerationSecretResolverResolvesVaultKVv1AndKVv2Responses(t *testing.T) {
	servers := []struct {
		body string
		want string
	}{
		{body: `{"data":{"password":"v1"}}`, want: "v1"},
		{body: `{"data":{"data":{"password":"v2"}}}`, want: "v2"},
	}
	resolver := newGenerationSecretResolverForTest(t)
	for index, test := range servers {
		t.Run(fmt.Sprintf("response-%d", index), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			fixture := newGenerationResolverFixture(
				t,
				generation.DomainHTTP,
				uint64(17+index),
				[]byte("route"),
				vaultConfigBytesForResolver(t, server.URL, "token"),
			)
			id := CandidateAttemptID(fixture.ticket, fixture.set)
			opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
			if err != nil {
				t.Fatal(err)
			}
			got, err := opened.ResolveScoped(
				context.Background(),
				resolverScope(fixture, id),
				"$secret://vault/test1/foo/password",
			)
			if err != nil || got != test.want {
				t.Fatalf("Vault value = %q/%v, want %q", got, err, test.want)
			}
		})
	}
}

func TestGenerationSecretResolverRejectsHTTPRedirectWithoutLeakingToken(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"redirect-target"}}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer redirect.Close()

	const token = "redirect-secret-token-must-not-leak"
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		18,
		[]byte("route"),
		vaultConfigBytesForResolver(t, redirect.URL, token),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	); !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), token) {
		t.Fatalf("redirect error = %v, want redacted ErrCredentialUnavailable", err)
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestGenerationSecretResolverRejectsUnsupportedManagerAndMalformedPath(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"must-not-read"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		19,
		[]byte("route"),
		vaultConfigBytesForResolver(t, server.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{
		"$secret://consul/test1/foo/password",
		"$secret://vault//foo/password",
		"$secret://vault/test1/password",
		"$secret://vault/test1/foo/",
	} {
		if _, err := opened.ResolveScoped(context.Background(), resolverScope(fixture, id), reference); err == nil {
			t.Fatalf("ResolveScoped(%q) error = nil", reference)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests for malformed references = %d, want 0", got)
	}
}

func TestGenerationSecretResolverUsesAttemptRetainedConfigNotStoreOrGlobalState(t *testing.T) {
	var retainedRequests atomic.Int32
	retainedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		retainedRequests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"retained"}}`))
	}))
	defer retainedServer.Close()
	var otherRequests atomic.Int32
	otherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherRequests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"other"}}`))
	}))
	defer otherServer.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		21,
		[]byte("route"),
		vaultConfigBytesForResolver(t, retainedServer.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	otherConfig := vaultConfigBytesForResolver(t, otherServer.URL, "token")
	attempt := opened.(*generationSecretAttempt)
	attempt.gate.Lock()
	attempt.resources[generation.DomainHTTP][fixture.secretKey] = otherConfig
	attempt.gate.Unlock()
	got, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	)
	if err != nil || got != "other" {
		t.Fatalf("retained config replacement value = %q/%v, want other", got, err)
	}
	if retainedRequests.Load() != 0 || otherRequests.Load() != 1 {
		t.Fatalf("backend requests = retained:%d other:%d, want 0/1", retainedRequests.Load(), otherRequests.Load())
	}
}

func TestGenerationSecretResolverErrorsRedactReferencesTokensBodiesAndValues(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 23, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	const envName = "APISIX_GO_GENERATION_REDACT_JSON"
	const plaintext = "generation-secret-plaintext-must-not-appear"
	t.Setenv(envName, `{"field":42,"credential":"`+plaintext+`"}`)
	if _, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$ENV://"+envName+"/field",
	); err == nil || strings.Contains(err.Error(), plaintext) ||
		strings.Contains(err.Error(), envName) {
		t.Fatalf("environment error = %v, want redacted failure", err)
	}

	partialReader := &resolverPartialErrorReadCloser{payload: []byte(`{"data":{"password":"` + plaintext + `"}}`)}
	attempt := opened.(*generationSecretAttempt)
	attempt.resolver.client = &http.Client{
		Transport: resolverRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: partialReader, Header: make(http.Header)}, nil
		}),
	}
	configKey := generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}
	attempt.gate.Lock()
	attempt.resources[generation.DomainHTTP][configKey] = vaultConfigBytesForResolver(
		t,
		"http://vault.example.invalid",
		"secret-token",
	)
	attempt.gate.Unlock()
	_, err = opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	)
	if err == nil || strings.Contains(err.Error(), plaintext) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("partial body error = %v, want redacted failure", err)
	}
	if !allZeroResolverBytes(partialReader.seen) {
		t.Fatalf("partial response bytes were not zeroed: %q", partialReader.seen)
	}
}

func TestGenerationSecretResolverCacheZeroesEvictedExpiredAndClearedValues(t *testing.T) {
	cache := newGenerationAttemptSecretCache()
	now := time.Now()
	captured := make([][]byte, 0, generationVaultSecretCacheCapacity+2)
	for index := range generationVaultSecretCacheCapacity + 1 {
		var key generationAttemptSecretCacheKey
		key[0] = byte(index)
		key[1] = byte(index >> 8)
		cache.set(key, fmt.Sprintf("cache-secret-%d", index), time.Hour, now)
		captured = append(captured, cache.entries[key].value)
	}
	if !allZeroResolverBytes(captured[0]) {
		t.Fatal("evicted cache value was not zeroed")
	}

	var expiredKey generationAttemptSecretCacheKey
	expiredKey[0] = 0xff
	expiredKey[1] = 0xff
	cache.set(expiredKey, "expired-cache-secret", time.Millisecond, now)
	expired := cache.entries[expiredKey].value
	if _, ok := cache.get(expiredKey, now.Add(2*time.Millisecond)); ok {
		t.Fatal("expired cache value was returned")
	}
	if !allZeroResolverBytes(expired) {
		t.Fatal("expired cache value was not zeroed")
	}

	cache.clear()
	for index, value := range captured {
		if !allZeroResolverBytes(value) {
			t.Fatalf("cache value %d was not zeroed by clear", index)
		}
	}
	if len(cache.entries) != 0 {
		t.Fatalf("cache entries after clear = %d, want 0", len(cache.entries))
	}
}

func TestGenerationSecretResolverAttemptCloseZeroesRetainedAndCachedBytes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"password":"close-secret"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		25,
		[]byte("route"),
		vaultConfigBytesForResolver(t, server.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	attempt := opened.(*generationSecretAttempt)
	attempt.gate.RLock()
	retained := attempt.resources[fixture.scope.Domain][fixture.secretKey]
	attempt.gate.RUnlock()
	if _, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	); err != nil {
		t.Fatal(err)
	}
	cacheKey := newGenerationAttemptSecretCacheKey(retained, "token", "vault/test1", "foo/password")
	attempt.cache.mu.Lock()
	entry, ok := attempt.cache.entries[cacheKey]
	attempt.cache.mu.Unlock()
	if !ok {
		t.Fatal("resolved Vault value was not cached")
	}
	cached := entry.value
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !allZeroResolverBytes(retained) {
		t.Fatal("retained Vault configuration was not zeroed")
	}
	if !allZeroResolverBytes(cached) {
		t.Fatal("cached Vault value was not zeroed")
	}
	if attempt.resources != nil || len(attempt.cache.entries) != 0 {
		t.Fatal("attempt retained resources after close")
	}
}

func TestGenerationSecretResolverAttemptCloseDetachesAndWaitsForInflightResolve(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"inflight-secret"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		27,
		[]byte("route"),
		vaultConfigBytesForResolverTimeout(t, server.URL, "token", 30),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	scope := resolverScope(fixture, id)
	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := opened.ResolveScoped(context.Background(), scope, "$secret://vault/test1/foo/password")
		resolveDone <- resolveErr
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- opened.Close(context.Background()) }()
	detached := make(chan struct{})
	go func() {
		for {
			resolver.mu.Lock()
			_, live := resolver.attempts[id]
			_, draining := resolver.draining[id]
			resolver.mu.Unlock()
			if !live && draining {
				close(detached)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	<-detached
	if _, err := opened.ResolveScoped(
		context.Background(),
		scope,
		"$secret://vault/test1/foo/password",
	); !errors.Is(err, ErrCapabilityScopeMismatch) &&
		!errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("resolve after detach error = %v, want closed or scope-mismatch error", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight resolve completed: %v", err)
	default:
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("in-flight resolve error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close error = %v", err)
	}
}

func TestGenerationSecretResolverRejectsResolveAfterAttemptClose(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"post-close-secret"}}`))
	}))
	defer server.Close()
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		29,
		[]byte("route"),
		vaultConfigBytesForResolver(t, server.URL, "token"),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := opened.ResolveScoped(
		context.Background(),
		resolverScope(fixture, id),
		"$secret://vault/test1/foo/password",
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("resolve after close error = %v, want ErrCredentialUnavailable", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("backend requests after close = %d, want 0", got)
	}
}

func TestGenerationSecretResolverAllowsReopenOnlyAfterSuccessfulCleanup(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 31, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	attempt := opened.(*generationSecretAttempt)
	attempt.gate.RLock()
	oldRoute := attempt.resources[fixture.scope.Domain][fixture.routeKey]
	attempt.gate.RUnlock()
	if err := opened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !allZeroResolverBytes(oldRoute) {
		t.Fatal("old attempt resource was not zeroed")
	}
	reopened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatalf("reopen after cleanup error = %v", err)
	}
	newAttempt := reopened.(*generationSecretAttempt)
	newAttempt.gate.RLock()
	newRoute := newAttempt.resources[fixture.scope.Domain][fixture.routeKey]
	newAttempt.gate.RUnlock()
	if len(newRoute) == 0 || allZeroResolverBytes(newRoute) {
		t.Fatal("reopened attempt did not retain fresh resource bytes")
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationSecretResolverAttemptCloseIsIdempotent(t *testing.T) {
	resolver := newGenerationSecretResolverForTest(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 33, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	closeErrors := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			closeErrors <- opened.Close(context.Background())
		}()
	}
	group.Wait()
	close(closeErrors)
	for closeErr := range closeErrors {
		if closeErr != nil {
			t.Fatalf("concurrent Close error = %v", closeErr)
		}
	}
	for range callers {
		if err := opened.Close(context.Background()); err != nil {
			t.Fatalf("repeated Close error = %v", err)
		}
	}
	resolver.mu.Lock()
	_, live := resolver.attempts[id]
	_, draining := resolver.draining[id]
	resolver.mu.Unlock()
	if live || draining {
		t.Fatal("closed attempt remained registered")
	}
}

func TestGenerationSecretResolverClosePreventsNewOpensAndClosesAttemptsOnce(t *testing.T) {
	resolver, transport := newGenerationSecretResolverWithTrackingTransport(t)
	first := newGenerationResolverFixture(t, generation.DomainHTTP, 35, []byte("first-route"), nil)
	second := newGenerationResolverFixture(t, generation.DomainHTTP, 36, []byte("second-route"), nil)
	firstID := CandidateAttemptID(first.ticket, first.set)
	secondID := CandidateAttemptID(second.ticket, second.set)
	firstAttempt, err := resolver.OpenCandidate(context.Background(), firstID, first.ticket, first.set)
	if err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := resolver.OpenCandidate(context.Background(), secondID, second.ticket, second.set)
	if err != nil {
		t.Fatal(err)
	}
	firstConcrete := firstAttempt.(*generationSecretAttempt)
	secondConcrete := secondAttempt.(*generationSecretAttempt)
	if err := resolver.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !transport.closed.Load() || transport.closeCalls.Load() != 1 {
		t.Fatalf(
			"client cleanup state = closed:%t calls:%d, want true/1",
			transport.closed.Load(),
			transport.closeCalls.Load(),
		)
	}
	if firstConcrete.resources != nil || secondConcrete.resources != nil {
		t.Fatal("factory close left attempt resources retained")
	}
	if _, err := resolver.OpenCandidate(
		context.Background(),
		firstID,
		first.ticket,
		first.set,
	); !errors.Is(
		err,
		ErrCredentialUnavailable,
	) {
		t.Fatalf("open after factory close error = %v, want ErrCredentialUnavailable", err)
	}
	if err := resolver.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.closeCalls.Load() != 1 {
		t.Fatalf("repeated factory close calls = %d, want 1", transport.closeCalls.Load())
	}
}

func TestGenerationSecretResolverCloseOrdersAttemptAndClientCleanup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"ordered-secret"}}`))
	}))
	defer server.Close()
	resolver, transport := newGenerationSecretResolverWithTrackingTransport(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		37,
		[]byte("route"),
		vaultConfigBytesForResolverTimeout(t, server.URL, "token", 30),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	attempt := opened.(*generationSecretAttempt)
	attempt.gate.RLock()
	retained := attempt.resources[fixture.scope.Domain][fixture.secretKey]
	attempt.gate.RUnlock()
	var zeroBeforeClientClose atomic.Bool
	transport.beforeClose = func() { zeroBeforeClientClose.Store(allZeroResolverBytes(retained)) }

	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := opened.ResolveScoped(
			context.Background(),
			resolverScope(fixture, id),
			"$secret://vault/test1/foo/password",
		)
		resolveDone <- resolveErr
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- resolver.Close(context.Background()) }()
	waitForGenerationAttemptDraining(t, resolver, id)
	if transport.closed.Load() {
		t.Fatal("client closed before attempt cleanup")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("factory Close returned before in-flight attempt completed: %v", err)
	default:
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("in-flight resolve error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("factory Close error = %v", err)
	}
	if !zeroBeforeClientClose.Load() || !transport.closed.Load() || transport.closeCalls.Load() != 1 {
		t.Fatalf(
			"cleanup order = zero-before-close:%t closed:%t calls:%d, want true/true/1",
			zeroBeforeClientClose.Load(),
			transport.closed.Load(),
			transport.closeCalls.Load(),
		)
	}
}

func TestGenerationSecretResolverCloseIsIdempotent(t *testing.T) {
	resolver, transport := newGenerationSecretResolverWithTrackingTransport(t)
	fixture := newGenerationResolverFixture(t, generation.DomainHTTP, 39, []byte("route"), nil)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	if _, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	results := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			results <- resolver.Close(context.Background())
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent factory Close error = %v", err)
		}
	}
	for range callers {
		if err := resolver.Close(context.Background()); err != nil {
			t.Fatalf("repeated factory Close error = %v", err)
		}
	}
	if transport.closeCalls.Load() != 1 {
		t.Fatalf("factory client cleanup calls = %d, want 1", transport.closeCalls.Load())
	}
}

func TestGenerationSecretResolverCloseWaitsForInflightAttemptBeforeClientClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"wait-secret"}}`))
	}))
	defer server.Close()
	resolver, transport := newGenerationSecretResolverWithTrackingTransport(t)
	fixture := newGenerationResolverFixture(
		t,
		generation.DomainHTTP,
		41,
		[]byte("route"),
		vaultConfigBytesForResolverTimeout(t, server.URL, "token", 30),
	)
	id := CandidateAttemptID(fixture.ticket, fixture.set)
	opened, err := resolver.OpenCandidate(context.Background(), id, fixture.ticket, fixture.set)
	if err != nil {
		t.Fatal(err)
	}
	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := opened.ResolveScoped(
			context.Background(),
			resolverScope(fixture, id),
			"$secret://vault/test1/foo/password",
		)
		resolveDone <- resolveErr
	}()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- resolver.Close(context.Background()) }()
	waitForGenerationAttemptDraining(t, resolver, id)
	if transport.closed.Load() {
		t.Fatal("client closed while request was in flight")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("factory Close returned while request was in flight: %v", err)
	default:
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("in-flight resolve error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("factory Close error = %v", err)
	}
	if !transport.closed.Load() || transport.closeCalls.Load() != 1 {
		t.Fatalf(
			"client cleanup state = closed:%t calls:%d, want true/1",
			transport.closed.Load(),
			transport.closeCalls.Load(),
		)
	}
}
