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
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &requestValidationCompilerSecretBroker{values: map[string]string{raw: plaintext}}
	factory, err := NewWorkerCompilerFactory(
		workerTestEffective(),
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
		nil)
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
