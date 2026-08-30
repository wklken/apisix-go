package compiler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type requestValidationCompilerSecretCall struct {
	scope secret.Scope
	raw   string
}

type requestValidationCompilerSecretBroker struct {
	mu      sync.Mutex
	values  map[string]string
	calls   []requestValidationCompilerSecretCall
	revokes int
}

func (*requestValidationCompilerSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*requestValidationCompilerSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *requestValidationCompilerSecretBroker) ResolveScoped(
	_ context.Context, scope secret.Scope, raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, requestValidationCompilerSecretCall{scope: scope, raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", fmt.Errorf("request-validation compiler fixture secret is unavailable")
	}
	return value, nil
}

func (broker *requestValidationCompilerSecretBroker) RevokeAttempt(
	context.Context, secret.AttemptID,
) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revokes++
	return nil
}

func (broker *requestValidationCompilerSecretBroker) snapshot() (
	[]requestValidationCompilerSecretCall, int,
) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]requestValidationCompilerSecretCall(nil), broker.calls...), broker.revokes
}

func TestWorkerFactoryUsesManifestRequestValidationDeclarationWithExactOccurrenceScope(t *testing.T) {
	const (
		revision  = uint64(309)
		routeID   = "request-validation-secret-route"
		raw       = "$ENV://REQUEST_VALIDATION_COMPILER_SCOPE"
		plaintext = "compiler-scoped-private-value"
	)
	manifest := mustManifest(t)
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &requestValidationCompilerSecretBroker{values: map[string]string{raw: plaintext}}
	factory, err := NewWorkerCompilerFactory(
		manifest,
		workerTestEffective(manifest),
		testutil.NewSecretMaterializer(broker, catalog),
		workerTestRuntimeObservers(),
	)
	if err != nil {
		t.Fatal(err)
	}
	factory.effective.Config.Plugins = []string{"request-validation"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("factory Close() error = %v", err)
		}
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	desired := mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue("routes", routeID, fmt.Sprintf(`{
"id":%q,"uri":"/validate","plugins":{"request-validation":{"body_schema":{
"type":"object","properties":{"token":{"const":%q}},"required":["token"]}}},
"upstream":{"type":"roundrobin","nodes":{%q:1}}}`, routeID, raw, upstreamURL.Host)),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("PrepareGeneration() error = %v", err)
	}
	defer func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Errorf("prepared Close() error = %v", err)
		}
	}()

	calls, revokes := broker.snapshot()
	wantScope := secret.Scope{
		Generation: revision,
		Attempt:    prepared.attempt.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     "request-validation",
		Resource:   generation.ResourceKey{Kind: "routes", ID: routeID},
		Source:     capability.SecretPluginConfig,
		Field:      "body_schema",
	}
	if len(calls) != 1 || calls[0] != (requestValidationCompilerSecretCall{scope: wantScope, raw: raw}) {
		t.Fatalf("scoped secret calls = %#v, want exact route occurrence %#v", calls, wantScope)
	}
	if revokes != 0 {
		t.Fatalf("attempt revoked during active generation: %d", revokes)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"http://example.test/validate",
		strings.NewReader(`{"token":"`+plaintext+`"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	prepared.HTTP().Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("prepared secret-backed route response = %d/%q", response.Code, response.Body.String())
	}
}

func TestWorkerFactorySharesRequestValidationCompileLimitAcrossAttemptBindings(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_COMPILER_SHARED_LIMIT"
		plaintext = "compiler-shared-private-value"
	)
	manifest := mustManifest(t)
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &requestValidationCompilerSecretBroker{values: map[string]string{raw: plaintext}}
	factory, err := NewWorkerCompilerFactory(
		manifest,
		workerTestEffective(manifest),
		testutil.NewSecretMaterializer(broker, catalog),
		workerTestRuntimeObservers(),
	)
	if err != nil {
		t.Fatal(err)
	}
	factory.effective.Config.Plugins = []string{"request-validation"}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("factory Close() error = %v", err)
		}
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	prepare := func(revision uint64, count int) *PreparedGeneration {
		resources := make([]generation.Resource, count)
		for index := range count {
			routeID := fmt.Sprintf("request-validation-shared-%d-%d", revision, index)
			resources[index] = resourceValue("routes", routeID, fmt.Sprintf(`{
"id":%q,"uri":%q,"plugins":{"request-validation":{"body_schema":{
"type":"string","const":%q}}},"upstream":{"type":"roundrobin","nodes":{%q:1}}}`,
				routeID, fmt.Sprintf("/validate-%d", index), raw, upstreamURL.Host,
			))
		}
		desired := mustGenerationSnapshot(t, revision, resources, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatalf("PrepareGeneration(%d) error = %v", revision, err)
		}
		return prepared
	}

	first := prepare(310, 5)
	defer func() {
		if err := first.Close(context.Background()); err != nil {
			t.Errorf("first prepared Close() error = %v", err)
		}
	}()
	limiter, err := first.attempt.capability.SharedLimiter(
		"request-validation/sensitive-schema-compile", 4,
	)
	if err != nil {
		t.Fatal(err)
	}
	releases := make([]func(), 0, 4)
	for range 4 {
		release, err := limiter.Acquire(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	second := prepare(311, 1)
	defer func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("second prepared Close() error = %v", err)
		}
	}()
	secondRequest := httptest.NewRequest(
		http.MethodPost, "http://example.test/validate-0", strings.NewReader(`"`+plaintext+`"`),
	)
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	second.HTTP().Handler().ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusNoContent {
		t.Fatalf("different-attempt response = %d/%q", secondResponse.Code, secondResponse.Body.String())
	}

	type pendingRequest struct {
		cancel   context.CancelFunc
		response *httptest.ResponseRecorder
		done     chan struct{}
	}
	pending := make([]pendingRequest, 5)
	for index := range pending {
		requestCtx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(
			http.MethodPost,
			fmt.Sprintf("http://example.test/validate-%d", index),
			strings.NewReader(`"`+plaintext+`"`),
		).WithContext(requestCtx)
		request.Header.Set("Content-Type", "application/json")
		pending[index] = pendingRequest{
			cancel: cancel, response: httptest.NewRecorder(), done: make(chan struct{}),
		}
		go func(item pendingRequest) {
			first.HTTP().Handler().ServeHTTP(item.response, request)
			close(item.done)
		}(pending[index])
	}
	for index := range pending {
		select {
		case <-pending[index].done:
			t.Fatalf("route binding %d bypassed attempt-wide compile limit", index)
		case <-time.After(20 * time.Millisecond):
		}
	}
	for index := range pending {
		pending[index].cancel()
		select {
		case <-pending[index].done:
		case <-time.After(2 * time.Second):
			t.Fatalf("route binding %d did not wake after cancellation", index)
		}
		if pending[index].response.Code != http.StatusServiceUnavailable {
			t.Fatalf(
				"route binding %d cancellation response = %d, want 503",
				index, pending[index].response.Code,
			)
		}
	}
}
