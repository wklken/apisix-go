package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/util"
)

type independentEffectiveBindingIdentityConfig struct {
	PluginConfig    resource.PluginConfig                      `json:"plugin_config"`
	Source          independentEffectiveBindingSourceIdentity  `json:"source"`
	ResourceContext independentEffectiveBindingContextIdentity `json:"resource_context"`
}

type independentEffectiveBindingSourceIdentity struct {
	Kind     uint8                              `json:"kind"`
	Source   capability.SecretDeclarationSource `json:"source"`
	Resource generation.ResourceKey             `json:"resource"`
}

type independentEffectiveBindingContextIdentity struct {
	Kind        uint8                `json:"kind"`
	Route       resource.Route       `json:"route"`
	Service     resource.Service     `json:"service"`
	StreamRoute resource.StreamRoute `json:"stream_route"`
}

func TestServerShutdownPreservesExactGenerationOwnersThroughRealChain(t *testing.T) {
	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	var releaseDeliveryOnce sync.Once
	release := func() { releaseDeliveryOnce.Do(func() { close(releaseDelivery) }) }
	clickhouse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-deliveryStarted:
		default:
			close(deliveryStarted)
		}
		<-releaseDelivery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		release()
		clickhouse.Close()
	})

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
			Plugins: []string{"clickhouse-logger"},
		},
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest,
		effective,
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{resolver: resolver}
	engine, err := NewGenerationEngine(server, factory)
	if err != nil {
		t.Fatal(err)
	}
	server.engine = engine
	if err := engine.InstallRecovery(context.Background(), generation.RecoveryState{}); err != nil {
		t.Fatal(err)
	}

	pluginConfig := map[string]any{
		"endpoint_addr":       clickhouse.URL,
		"user":                "logger",
		"password":            "task8-private-password",
		"database":            "logs",
		"logtable":            "access_log",
		"log_format":          map[string]any{"route_id": "$route_id"},
		"timeout":             60,
		"batch_max_size":      1,
		"buffer_duration":     60,
		"inactive_timeout":    60,
		"max_pending_entries": 1,
	}
	routeValue := map[string]any{
		"id":      "clickhouse-log-route",
		"uri":     "/clickhouse-log",
		"plugins": map[string]any{"clickhouse-logger": pluginConfig},
		"upstream": map[string]any{
			"type":  "roundrobin",
			"nodes": map[string]any{"127.0.0.1:19876": 1},
		},
	}
	rawRoute, err := apisixjson.Marshal(routeValue)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := generation.NewSnapshot(801, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "routes", ID: "clickhouse-log-route"}, Value: rawRoute,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: desired.Revision(),
		DesiredDigest:   desired.Digest(),
		Cursor: generation.ProviderCursor{
			Provider: "task8-server-chain", Revision: "801",
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set, err := engine.Prepare(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := generation.PublicationToken("task8-server-chain")
	if err := engine.Activate(context.Background(), token, set); err != nil {
		t.Fatal(err)
	}
	engine.FinalizeActivation(context.Background(), token, set)
	active := engine.active.Load().http
	if active == nil || active.prepared == nil {
		t.Fatal("activated HTTP generation is unavailable")
	}
	if quarantined := active.prepared.HTTP().Quarantined(); len(quarantined) != 0 {
		t.Fatalf(
			"activated HTTP generation quarantined routes = %v decisions = %v",
			quarantined, set.Domains[generation.DomainHTTP].Decisions,
		)
	}

	ownerPrefix := independentClickHouseOwnerPrefix(t, manifest, ticket, set, rawRoute)
	clientKey := independentClickHouseClientKey(
		t,
		clickhouse.URL,
		"logger",
		"task8-private-password",
		"logs",
		60,
	)
	var probeCreates atomic.Int32
	var probeCloses atomic.Int32
	acquireProbe := func() func() {
		t.Helper()
		_, releaseProbe, acquireErr := shared.AcquireClient(
			clientKey,
			func() (any, error) {
				probeCreates.Add(1)
				return resty.New(), nil
			},
			func(any) { probeCloses.Add(1) },
		)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		return releaseProbe
	}
	response := httptest.NewRecorder()
	server.routes.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "http://gateway.test/clickhouse-log", nil),
	)
	if response.Code == http.StatusNotFound || response.Code == http.StatusServiceUnavailable {
		t.Fatalf("route response status = %d body = %q", response.Code, response.Body.String())
	}
	select {
	case <-deliveryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ClickHouse delivery did not start")
	}
	activeProbeRelease := acquireProbe()
	activeProbeRelease()
	if got := probeCreates.Load(); got != 0 {
		t.Fatalf("active generation client probe created %d clients, want shared reuse", got)
	}

	want := []runtime.TaskResidual{
		{Owner: ownerPrefix + "/batch-shutdown"},
		{Owner: ownerPrefix + "/batch-worker"},
	}
	shortCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shutdownErr := server.Shutdown(shortCtx)
	var residualErr *runtime.TaskResidualError
	if !errors.As(shutdownErr, &residualErr) ||
		!errors.Is(shutdownErr, compiler.ErrPreparedGenerationCleanupIncomplete) {
		t.Fatalf(
			"Server.Shutdown() error = %v, want residual and incomplete cleanup",
			shutdownErr,
		)
	}
	if got := residualErr.Residuals(); !reflect.DeepEqual(got, want) {
		t.Fatalf("residuals = %v, want %v", got, want)
	}
	for _, residual := range residualErr.Residuals() {
		if residual.Owner == ownerPrefix+"/batch-scheduler" {
			t.Fatalf("forbidden residual retained: %v", residualErr.Residuals())
		}
	}
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Server.Shutdown() error = %v, want deadline exceeded", shutdownErr)
	}
	if server.engineClosed || server.resolverClosed || server.shutdownComplete {
		t.Fatalf("incomplete shutdown released later owners: engine=%t resolver=%t complete=%t",
			server.engineClosed, server.resolverClosed, server.shutdownComplete)
	}
	retainedProbeRelease := acquireProbe()
	retainedProbeRelease()
	if got := probeCreates.Load(); got != 0 {
		t.Fatalf("incomplete shutdown released client resource; probe creates = %d", got)
	}

	release()
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Server.Shutdown() error = %v", err)
	}
	if !server.engineClosed || !server.resolverClosed || !server.shutdownComplete {
		t.Fatalf("terminal shutdown state: engine=%t resolver=%t complete=%t",
			server.engineClosed, server.resolverClosed, server.shutdownComplete)
	}
	terminalProbeRelease := acquireProbe()
	if got := probeCreates.Load(); got != 1 {
		t.Fatalf("terminal shutdown client probe creates = %d, want 1 after release", got)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("terminal Server.Shutdown() replay error = %v", err)
	}
	if got := probeCloses.Load(); got != 0 {
		t.Fatalf("terminal replay closed replacement client %d times, want 0", got)
	}
	terminalProbeRelease()
	terminalProbeRelease()
	if got := probeCloses.Load(); got != 1 {
		t.Fatalf("replacement client cleanup count = %d, want 1", got)
	}
}

func independentClickHouseClientKey(
	t *testing.T,
	endpoint, user, password, database string,
	timeout int,
) string {
	t.Helper()
	secretIdentity := func(value string) string {
		digest := sha256.Sum256([]byte(value))
		descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
		if err != nil {
			t.Fatal(err)
		}
		return descriptor.String()
	}
	uid := shared.NewConfigUID()
	uid.Add(endpoint)
	uid.Add(secretIdentity(user))
	uid.Add(secretIdentity(password))
	uid.Add(database)
	uid.Add(timeout)
	uid.Add(true)
	return shared.ClientKey("clickhouse-logger", uid)
}

func independentClickHouseOwnerPrefix(
	t *testing.T,
	manifest *capability.Manifest,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
	rawRoute []byte,
) string {
	t.Helper()
	descriptor, err := plugin.DescriptorForFactory(manifest, "clickhouse-logger")
	if err != nil {
		t.Fatal(err)
	}
	var route resource.Route
	decoder := apisixjson.NewDecoder(bytes.NewReader(rawRoute))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(document, &route); err != nil {
		t.Fatal(err)
	}
	planned, err := routepkg.PlanHTTPPlugins(context.Background(), routepkg.PlanningInput{
		Routes: []resource.Route{route}, EnabledPlugins: []string{"clickhouse-logger"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Routes) != 1 || len(planned.Routes[0].Local) != 1 {
		t.Fatalf("planned clickhouse routes = %#v", planned.Routes)
	}
	plannedRoute := planned.Routes[0]
	local := plannedRoute.Local[0]
	if local.Factory != "clickhouse-logger" {
		t.Fatalf("planned local factory = %q", local.Factory)
	}
	routeKey := generation.ResourceKey{Kind: "routes", ID: "clickhouse-log-route"}
	identity := plugin.InstanceIdentityInput{PluginConfig: independentEffectiveBindingIdentityConfig{
		PluginConfig: local.Config,
		Source: independentEffectiveBindingSourceIdentity{
			Kind: 0, Source: capability.SecretPluginConfig, Resource: routeKey,
		},
		ResourceContext: independentEffectiveBindingContextIdentity{
			Kind: 1, Route: plannedRoute.Route, Service: plannedRoute.Service,
			StreamRoute: resource.StreamRoute{},
		},
	}, Filter: local.FilterIdentity, ErrorResponse: local.ErrorResponse}
	instance, err := plugin.NewAttemptInstanceKey(
		secret.CandidateAttemptID(ticket, set), descriptor, plugin.ScopeRoute,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeKey.ID}, identity,
	)
	if err != nil {
		t.Fatal(err)
	}
	var canonical bytes.Buffer
	for _, value := range []string{
		"apisix-go/plugin-task-owner/v1", instance.Factory,
	} {
		writeOwnerString(t, &canonical, value)
	}
	canonical.Write(instance.Attempt[:])
	canonical.WriteByte(byte(instance.Scope))
	writeOwnerString(t, &canonical, string(instance.Owner.Kind))
	writeOwnerString(t, &canonical, instance.Owner.ID)
	canonical.Write(instance.ConfigDigest[:])
	digest := sha256.Sum256(canonical.Bytes())
	return "plugin/" + strings.Trim(instance.Factory, "-") + "/" + hex.EncodeToString(digest[:])
}

func writeOwnerString(t *testing.T, target *bytes.Buffer, value string) {
	t.Helper()
	if uint64(len(value)) > math.MaxUint32 {
		t.Fatal("owner field length exceeds uint32")
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	target.Write(length[:])
	target.WriteString(value)
}
