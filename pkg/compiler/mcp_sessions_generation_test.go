package compiler

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestParityCompilerSuccessorReachesActiveMCPSession(t *testing.T) {
	registry := runtime.NewResourceRegistry()
	first, firstFixture := newEffectiveBindingMaterializerFixture(t, []string{"mcp-bridge"}, nil)
	second, secondFixture := newEffectiveBindingMaterializerFixture(t, []string{"mcp-bridge"}, nil)
	first.registry, second.registry = registry, registry
	materialize := func(prepared *PreparedGeneration, fixture *effectiveBindingMaterializerFixture) plugin.Binding {
		t.Helper()
		state, err := prepared.acquireHTTPMCPSessions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		spec := fixture.pluginSpec("mcp-bridge", "route-1")
		spec.config = map[string]any{"command": "cat"}
		spec.runtimeContext = effectiveBindingRuntimeContext{
			configured: true, enabledFactories: []string{"mcp-bridge"},
			publicAPIRegistry: public_api.NewRegistry(),
			runtimeAcquirer:   testTrafficSplitRuntimeAcquirer{},
			upstreamResolver:  func(string) (resource.Upstream, error) { return resource.Upstream{}, nil },
			mcpSessions:       state,
		}
		bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{spec})
		if err != nil || len(bindings) != 1 {
			t.Fatalf("materialize: %v, %v", bindings, err)
		}
		return bindings[0]
	}
	firstBinding := materialize(first, firstFixture)
	finished := make(chan struct{})
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		firstBinding.Plugin.Handler(nil).ServeHTTP(w, r)
	}))
	defer firstServer.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	stream, err := client.Get(firstServer.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	scanner := bufio.NewScanner(stream.Body)
	if !scanner.Scan() || scanner.Text() != "event: endpoint" {
		t.Fatalf("SSE endpoint event: %q %v", scanner.Text(), scanner.Err())
	}
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "data: /message?sessionId=") {
		t.Fatalf("SSE endpoint: %q", scanner.Text())
	}
	endpoint := strings.TrimPrefix(scanner.Text(), "data: ")
	secondBinding := materialize(second, secondFixture)
	secondServer := httptest.NewServer(secondBinding.Plugin.Handler(nil))
	defer secondServer.Close()
	const message = `{"jsonrpc":"2.0","method":"ping","id":42}`
	result, err := client.Post(secondServer.URL+endpoint, "application/json", strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, result.Body)
	_ = result.Body.Close()
	if result.StatusCode != http.StatusAccepted {
		t.Fatalf("successor message status=%d, want 202", result.StatusCode)
	}
	received := false
	for scanner.Scan() {
		if scanner.Text() == "data: "+message {
			received = true
			break
		}
	}
	if !received {
		t.Fatalf("old SSE did not receive successor message: %v", scanner.Err())
	}
	_ = stream.Body.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("SSE process failed to drain")
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One successor plugin instance and its shared session registry remain.
	if registry.Len() != 2 {
		t.Fatalf("successor lost shared registry: %d", registry.Len())
	}
	result, err = client.Post(secondServer.URL+endpoint, "application/json", strings.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	_ = result.Body.Close()
	if result.StatusCode != http.StatusInternalServerError {
		t.Fatalf("closed session still accepts messages: %d", result.StatusCode)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if registry.Len() != 0 {
		t.Fatalf("session registry retained after final generation: %d", registry.Len())
	}
}
