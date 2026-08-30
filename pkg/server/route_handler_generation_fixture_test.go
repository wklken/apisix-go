package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
)

type compiledHTTPGenerationFixture struct {
	engine   *GenerationEngine
	routes   *routeHandler
	resolver *secret.GenerationSecretResolver

	acquires atomic.Int64
	releases atomic.Int64
}

func newCompiledHTTPGenerationFixture(
	t *testing.T,
	revision uint64,
	plugins []string,
	resources []generation.Resource,
) *compiledHTTPGenerationFixture {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	encryption := data_encryption.NewService(false, nil, catalog)
	resolver, err := secret.NewGenerationSecretResolver(encryption)
	if err != nil {
		t.Fatal(err)
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{
			Plugins: plugins,
		},
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest,
		effective,
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	)
	if err != nil {
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}
	engine, err := NewGenerationEngine(&Server{}, factory)
	if err != nil {
		_ = factory.Close(context.Background())
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}
	desired, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		_ = engine.Close(context.Background())
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   desired.Digest(),
		Cursor: generation.ProviderCursor{
			Provider: "route-handler-test", Revision: strconv.FormatUint(revision, 10),
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	_, err = engine.Publish(context.Background(), ticket, desired, nil)
	if err != nil {
		_ = engine.Close(context.Background())
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}

	fixture := &compiledHTTPGenerationFixture{engine: engine, resolver: resolver}
	fixture.routes = newGenerationRouteHandler(fixture.acquire)
	t.Cleanup(func() {
		fixture.routes.Close()
		if err := fixture.engine.Close(context.Background()); err != nil {
			t.Errorf("GenerationEngine.Close() error = %v", err)
		}
		if err := fixture.resolver.Close(context.Background()); err != nil {
			t.Errorf("GenerationSecretResolver.Close() error = %v", err)
		}
	})
	return fixture
}

func (fixture *compiledHTTPGenerationFixture) acquire() (httpGenerationLease, bool) {
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		return httpGenerationLease{}, false
	}
	fixture.acquires.Add(1)
	return fixture.count(lease), true
}

func (fixture *compiledHTTPGenerationFixture) count(lease httpGenerationLease) httpGenerationLease {
	baseRelease := lease.Release
	baseRetain := lease.retain
	var releaseOnce sync.Once
	lease.Release = func() {
		releaseOnce.Do(func() {
			fixture.releases.Add(1)
			baseRelease()
		})
	}
	lease.retain = func() (httpGenerationLease, bool) {
		child, ok := baseRetain()
		if !ok {
			return httpGenerationLease{}, false
		}
		fixture.acquires.Add(1)
		return fixture.count(child), true
	}
	return lease
}

func compiledHTTPRouteResource(
	t *testing.T,
	id string,
	uri string,
	backend *httptest.Server,
	plugins map[string]resource.PluginConfig,
) generation.Resource {
	t.Helper()
	host, portText, err := net.SplitHostPort(backend.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	route := resource.Route{
		ID: id, Uri: uri, Plugins: plugins,
		Upstream: resource.Upstream{
			Scheme: "http", Nodes: []resource.Node{{Host: host, Port: port, Weight: 1}},
		},
	}
	raw, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	return generation.Resource{Key: generation.ResourceKey{Kind: "routes", ID: id}, Value: raw}
}
