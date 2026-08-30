package route

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type testLiteralSecretBroker struct{}

func (testLiteralSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (testLiteralSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by route plugin fixtures")
}

func (testLiteralSecretBroker) ResolveScoped(
	ctx context.Context,
	_ secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return raw, nil
}

func (testLiteralSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func testPluginInitializationError(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	zones ...[]appconfig.Zone,
) error {
	t.Helper()
	effective := testEffectiveConfig()
	if len(zones) > 0 {
		effective.Config.Apisix.ProxyCache.Zones = slices.Clone(zones[0])
	}
	instance := plugin.New(name, base.Dependencies{
		Config:         effective,
		DataEncryption: testDataEncryptionResolver(),
	})
	if instance == nil {
		return fmt.Errorf("plugin %q is not supported", name)
	}
	if err := instance.Init(); err != nil {
		return err
	}
	if err := util.Parse(config, instance.Config()); err != nil {
		return err
	}
	if setter, ok := instance.(interface{ SetConfiguredZones([]appconfig.Zone) }); ok {
		setter.SetConfiguredZones(slices.Clone(effective.Config.Apisix.ProxyCache.Zones))
	}
	capabilityValue, scope, cleanup := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	defer cleanup()
	if err := plugin.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, instance,
	); err != nil {
		return err
	}
	return instance.PostInit()
}

func testPreparedRoutes(routes ...resource.Route) []PreparedRoute {
	prepared := make([]PreparedRoute, len(routes))
	for index, routeResource := range routes {
		prepared[index] = PreparedRoute{
			Route: routeResource,
			Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusNoContent)
			}),
		}
	}
	return prepared
}

func testRouteFromJSON(t testing.TB, raw string) resource.Route {
	t.Helper()
	var routeResource resource.Route
	if err := json.Unmarshal([]byte(raw), &routeResource); err != nil {
		t.Fatalf("unmarshal route fixture: %v", err)
	}
	return routeResource
}

func testPreparedProxyHandler(
	t testing.TB,
	routeResource resource.Route,
	service resource.Service,
	effective *appconfig.EffectiveConfig,
	bindings ...plugin.Binding,
) http.Handler {
	t.Helper()
	return testPreparedProxyHandlerWithConsumers(
		t, routeResource, service, effective, nil, bindings...,
	)
}

func testPreparedProxyHandlerWithConsumers(
	t testing.TB,
	routeResource resource.Route,
	service resource.Service,
	effective *appconfig.EffectiveConfig,
	consumers map[string]PreparedConsumerRecord,
	bindings ...plugin.Binding,
) http.Handler {
	t.Helper()
	if routeResource.ID == "" {
		routeResource.ID = "prepared-proxy-test-route"
	}
	plan, err := PlanRouteUpstream(routeResource, service, nil, nil, &effective.Config)
	if err != nil {
		t.Fatalf("PlanRouteUpstream() error = %v", err)
	}
	if plan.ClusterConfig == nil {
		t.Fatal("PlanRouteUpstream() cluster config = nil")
	}
	cluster, err := pxy.NewCluster(*plan.ClusterConfig, pxy.NopClusterObserver{})
	if err != nil {
		t.Fatalf("NewCluster() error = %v", err)
	}
	t.Cleanup(cluster.Close)
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route:          routeResource,
		Service:        service,
		StaticBindings: bindings,
		Consumers:      consumers,
		Upstream:       plan,
		Runtime: PreparedUpstreamRuntime{
			LoadBalancer: cluster.LoadBalancer(), RoundTripper: cluster.RoundTripper(),
		},
		StaticConfig: effective.Config,
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}
	return handler
}

func testPreparedPluginHandler(
	t testing.TB,
	routeResource resource.Route,
	bindings ...plugin.Binding,
) http.Handler {
	t.Helper()
	if routeResource.ID == "" {
		routeResource.ID = "prepared-plugin-test-route"
	}
	handler, err := BuildPreparedHandler(PreparedHandlerInput{
		Route:          routeResource,
		StaticBindings: bindings,
		Runtime:        PreparedUpstreamRuntime{RoundTripper: http.DefaultTransport},
		StaticConfig:   testEffectiveConfig().Config,
	})
	if err != nil {
		t.Fatalf("BuildPreparedHandler() error = %v", err)
	}
	return handler
}

func testPluginBinding(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	routeResource resource.Route,
) plugin.Binding {
	t.Helper()
	return testPluginBindingForSource(
		t,
		name,
		config,
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
		routeResource,
		resource.Service{},
		"127.0.0.1:9080",
	)
}

func testScopedSecretPluginBinding(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	routeResource resource.Route,
) plugin.Binding {
	t.Helper()
	const revision = uint64(1)
	key := generation.ResourceKey{Kind: "routes", ID: routeResource.ID}
	document, err := json.Marshal(map[string]any{
		"id":      routeResource.ID,
		"plugins": map[string]resource.PluginConfig{name: config},
	})
	if err != nil {
		t.Fatalf("marshal plugin %q occurrence: %v", name, err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{Key: key, Value: document}}, nil)
	if err != nil {
		t.Fatalf("build plugin %q snapshot: %v", name, err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
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
					Key: key, Disposition: generation.DispositionPublished, Code: "route-test",
				}},
			},
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatalf("build secret catalog: %v", err)
	}
	registration, err := secret.NewScopedMaterializer(testLiteralSecretBroker{}, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatalf("register plugin %q attempt: %v", name, err)
	}
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close plugin %q attempt: %v", name, err)
		}
	})
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatalf("build plugin %q capability: %v", name, err)
	}
	instance := plugin.New(name, base.Dependencies{
		Config:  testEffectiveConfig(),
		Secrets: capabilityValue,
	})
	if instance == nil {
		t.Fatalf("plugin %q is not supported", name)
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("plugin %q Init() error = %v", name, err)
	}
	if err := util.Parse(config, instance.Config()); err != nil {
		t.Fatalf("plugin %q config error = %v", name, err)
	}
	if setter, ok := instance.(interface{ SetConfiguredZones([]appconfig.Zone) }); ok {
		setter.SetConfiguredZones(nil)
	}
	if err := plugin.MaterializeScopedPluginSecrets(
		context.Background(),
		secret.Scope{
			Generation: revision,
			Attempt:    registration.AttemptID(),
			Domain:     generation.DomainHTTP,
			Plugin:     name,
			Resource:   key,
			Source:     capability.SecretPluginConfig,
		},
		capabilityValue,
		instance,
	); err != nil {
		t.Fatalf("plugin %q secret preparation error = %v", name, err)
	}
	if setter, ok := instance.(interface{ SetRouteContext(string, string) }); ok {
		setter.SetRouteContext(routeResource.ID, "127.0.0.1:9080")
	}
	if setter, ok := instance.(interface {
		SetResourceContext(resource.Route, resource.Service)
	}); ok {
		setter.SetResourceContext(routeResource, resource.Service{})
	}
	if err := instance.PostInit(); err != nil {
		t.Fatalf("plugin %q PostInit() error = %v", name, err)
	}
	if stopper, ok := instance.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	binding, err := plugin.BindPluginChecked(
		name,
		instance,
		plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", name, err)
	}
	return binding
}

func testPluginBindingForSource(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
	routeResource resource.Route,
	service resource.Service,
	serverAddr string,
) plugin.Binding {
	t.Helper()
	return testPluginBindingForSourceWithDependencies(
		t,
		name,
		config,
		scope,
		provenance,
		routeResource,
		service,
		serverAddr,
		base.Dependencies{Config: testEffectiveConfig()},
	)
}

func testPluginBindingForSourceWithDependencies(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	scope plugin.Scope,
	provenance plugin.ResourceProvenance,
	routeResource resource.Route,
	service resource.Service,
	serverAddr string,
	dependencies base.Dependencies,
) plugin.Binding {
	t.Helper()
	if dependencies.Tasks == nil {
		registry := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
			t.Errorf("plugin fixture task %q failed: %v", failure.Owner, failure.Err)
		})
		owner, err := runtime.NewTaskOwner(registry, "plugin/test/route-fixture", runtime.TaskPlugin)
		if err != nil {
			t.Fatalf("NewTaskOwner() error = %v", err)
		}
		dependencies.Tasks = owner
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			residuals, stopErr := registry.Stop(ctx)
			if stopErr != nil {
				t.Errorf("stop plugin fixture tasks: %v (residuals=%v)", stopErr, residuals)
			}
		})
	}
	instance := plugin.New(name, dependencies)
	if instance == nil {
		t.Fatalf("plugin %q is not supported", name)
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("plugin %q Init() error = %v", name, err)
	}
	if err := util.Parse(config, instance.Config()); err != nil {
		t.Fatalf("plugin %q config error = %v", name, err)
	}
	if setter, ok := instance.(interface{ SetConfiguredZones([]appconfig.Zone) }); ok {
		var zones []appconfig.Zone
		if dependencies.Config != nil {
			zones = dependencies.Config.Config.Apisix.ProxyCache.Zones
		}
		setter.SetConfiguredZones(slices.Clone(zones))
	}
	if setter, ok := instance.(interface{ SetRouteContext(string, string) }); ok {
		setter.SetRouteContext(routeResource.ID, serverAddr)
	}
	if setter, ok := instance.(interface {
		SetResourceContext(resource.Route, resource.Service)
	}); ok {
		setter.SetResourceContext(routeResource, service)
	}
	if materializer, ok := instance.(interface {
		MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error
	}); ok {
		if err := materializer.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
			t.Fatalf("plugin %q secret preparation error = %v", name, err)
		}
	}
	if err := instance.PostInit(); err != nil {
		t.Fatalf("plugin %q PostInit() error = %v", name, err)
	}
	if stopper, ok := instance.(interface{ Stop() }); ok {
		t.Cleanup(stopper.Stop)
	}
	binding, err := plugin.BindPluginChecked(
		name,
		instance,
		scope,
		provenance,
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", name, err)
	}
	return binding
}

func testPlannedPluginBinding(
	t testing.TB,
	name string,
	config resource.PluginConfig,
	routeResource resource.Route,
) plugin.Binding {
	t.Helper()
	provenance := plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID}
	plans, err := planPluginSources(
		materializedPluginSources(
			map[string]resource.PluginConfig{name: config},
			provenance,
		),
		plugin.NewEnabledSet([]string{name}),
		false,
	)
	if err != nil {
		t.Fatalf("plan plugin %q: %v", name, err)
	}
	if len(plans) != 1 {
		t.Fatalf("plugin %q plans = %d, want 1", name, len(plans))
	}
	binding := testPluginBindingForSource(
		t,
		name,
		plans[0].Config,
		plugin.ScopeRoute,
		provenance,
		routeResource,
		resource.Service{},
		"127.0.0.1:9080",
	)
	binding, err = plans[0].Apply(binding)
	if err != nil {
		t.Fatalf("apply plugin %q plan: %v", name, err)
	}
	return binding
}

func testEffectiveConfig() *appconfig.EffectiveConfig {
	static := appconfig.Config{
		Apisix: appconfig.Apisix{
			NodeListen: []appconfig.NodeListen{{Port: 9080}},
			ProxyMode:  "http",
		},
		NginxConfig: appconfig.NginxConfig{HTTP: appconfig.NginxHTTP{
			ClientMaxBodySize: 10 * 1024 * 1024,
			ClientBodyTimeout: 60 * time.Second,
		}},
		Proxy: appconfig.Proxy{
			MaxIdleConns: 1024, MaxIdleConnsPerHost: 256,
			MaxConnsPerHost: 512, MaxInFlight: 1024,
		},
		Plugins: []string{"request-id"},
		Deployment: appconfig.Deployment{
			Role:          "data_plane",
			RoleDataPlane: appconfig.RoleConfig{ConfigProvider: "yaml"},
		},
	}
	root := filepath.Join(os.TempDir(), "apisix-go-route-test")
	return &appconfig.EffectiveConfig{
		Config: static,
		Paths: appconfig.RuntimePaths{
			DataDir: filepath.Join(root, "data"), RuntimeDir: filepath.Join(root, "run"),
			LogDir: filepath.Join(root, "log"), TempDir: filepath.Join(root, "tmp"),
		},
	}
}

func testDataEncryptionResolver() data_encryption.Resolver {
	return data_encryption.NewResolver(false, nil)
}
