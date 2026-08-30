package server

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

type generationContractFixture struct {
	engine   *GenerationEngine
	resolver *secret.GenerationSecretResolver
}

type generationRequestObservation struct {
	revision       uint64
	consumerID     string
	consumerLogin  string
	consumerSecret string
	metadata       []byte
	response       string
}

func TestGenerationEngineOldAndNewRequestsUseOwnConsumerMetadataProtoAndSecrets(t *testing.T) {
	t.Setenv("TASK9_OLD_LOGIN", "old-login")
	t.Setenv("TASK9_NEW_LOGIN", "new-login")
	t.Setenv("TASK9_OLD_PASSWORD", "old-password")
	t.Setenv("TASK9_NEW_PASSWORD", "new-password")

	oldLogPath := t.TempDir() + "/old.log"
	newLogPath := t.TempDir() + "/new.log"
	oldBackendObservations := make(chan generationRequestObservation, 1)
	newBackendObservations := make(chan generationRequestObservation, 1)
	oldBackend := httptest.NewServer(generationContractGRPCBackend(oldBackendObservations))
	t.Cleanup(oldBackend.Close)
	newBackend := httptest.NewServer(generationContractGRPCBackend(newBackendObservations))
	t.Cleanup(newBackend.Close)
	fixture := newGenerationContractFixture(t, false)

	prepareGenerationContract(t, fixture.engine, 201, generationContractResources(
		t, oldBackend.URL, "old", "TASK9_OLD_LOGIN", "TASK9_OLD_PASSWORD", oldLogPath, nil,
	))
	oldOwner := fixture.engine.active.Load().http
	if quarantined := oldOwner.prepared.HTTP().Quarantined(); len(quarantined) != 0 {
		t.Fatalf("old generation quarantined routes = %v", quarantined)
	}

	oldLease, ok := fixture.engine.acquireHTTP()
	if !ok || oldLease.Snapshot != oldOwner.prepared.HTTP() {
		t.Fatal("old request did not acquire the predecessor generation")
	}
	oldRevision := oldLease.Snapshot.Revision()
	oldLeaseAcquired := make(chan struct{})
	releaseOldExecution := make(chan struct{})
	var releaseOldExecutionOnce sync.Once
	releaseOldRequest := func() { releaseOldExecutionOnce.Do(func() { close(releaseOldExecution) }) }
	defer releaseOldRequest()
	close(oldLeaseAcquired)
	oldResponse := httptest.NewRecorder()
	oldRequest := generationContractRequest("old")
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		defer oldLease.Release()
		<-releaseOldExecution
		serveRouteRequestForHTTPGeneration(
			oldResponse, oldRequest, oldLease.Snapshot.Handler(), &oldLease, nil,
		)
	}()
	receiveContractSignal(t, oldLeaseAcquired, "old request lease barrier")
	oldOwner.mu.Lock()
	oldLeases := oldOwner.leases
	oldOwner.mu.Unlock()
	if oldLeases == 0 {
		t.Fatal("old request reached its upstream without retaining the predecessor lease")
	}

	prepareGenerationContract(t, fixture.engine, 202, generationContractResources(
		t, newBackend.URL, "new", "TASK9_NEW_LOGIN", "TASK9_NEW_PASSWORD", newLogPath, nil,
	))
	newOwner := fixture.engine.active.Load().http
	if newOwner == nil || newOwner == oldOwner {
		t.Fatal("new publication did not install a distinct generation owner")
	}
	newRevision := newOwner.prepared.HTTP().Revision()

	routes := newGenerationRouteHandler(fixture.engine.acquireHTTP)
	newResponse := httptest.NewRecorder()
	routes.ServeHTTP(newResponse, generationContractRequest("new"))
	releaseOldRequest()
	receiveContractSignal(t, oldDone, "old request completion")
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatalf("GenerationEngine.Close() error = %v", err)
	}
	oldObservation := receiveGenerationRequestObservation(t, oldBackendObservations, "old backend request")
	newObservation := receiveGenerationRequestObservation(t, newBackendObservations, "new backend request")
	oldObservation.revision = oldRevision
	newObservation.revision = newRevision
	oldObservation.metadata = readGenerationContractLog(t, oldLogPath)
	newObservation.metadata = readGenerationContractLog(t, newLogPath)
	oldObservation.response = oldResponse.Body.String()
	newObservation.response = newResponse.Body.String()
	wantOld := generationRequestObservation{
		revision: 201, consumerID: "old-consumer", consumerLogin: "old-login",
		consumerSecret: "old-password", metadata: []byte(`"generation":"old-metadata"`),
		response: `{"oldresult":"old-value"}`,
	}
	wantNew := generationRequestObservation{
		revision: 202, consumerID: "new-consumer", consumerLogin: "new-login",
		consumerSecret: "new-password", metadata: []byte(`"generation":"new-metadata"`),
		response: `{"newresult":"new-value"}`,
	}
	assertGenerationObservation(t, oldObservation, wantOld)
	assertGenerationObservation(t, newObservation, wantNew)
}

func TestGenerationEngineHijackedConnectionRetainsPredecessorResources(t *testing.T) {
	fixture := newGenerationContractFixture(t, false)
	prepareEngineGeneration(t, fixture.engine, 211, generation.DomainHTTP)
	oldOwner := fixture.engine.active.Load().http

	parent, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("predecessor HTTP lease unavailable")
	}
	routes := newGenerationRouteHandler(fixture.engine.acquireHTTP)
	left, right := net.Pipe()
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
	})
	var hijacked net.Conn
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var err error
		hijacked, _, err = w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("Hijack() error = %v", err)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&hijackingRouteResponseWriter{header: make(http.Header), conn: left},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	if hijacked == nil {
		t.Fatal("request did not return a generation-wrapped hijacked connection")
	}

	prepareEngineGeneration(t, fixture.engine, 212, generation.DomainHTTP)
	select {
	case <-oldOwner.closeDone:
		t.Fatal("predecessor closed while its hijacked connection was live")
	default:
	}
	if oldOwner.prepared.HTTP() == nil || oldOwner.prepared.ConsumerLookup() == nil {
		t.Fatal("predecessor resources were revoked before the hijacked connection closed")
	}

	if err := hijacked.Close(); err != nil {
		t.Fatalf("hijacked connection Close() error = %v", err)
	}
	receiveContractSignal(t, oldOwner.closeDone, "predecessor retirement")
	if oldOwner.prepared.HTTP() != nil || oldOwner.prepared.ConsumerLookup() != nil {
		t.Fatal("predecessor resources remained live after the hijacked connection drained")
	}
}

func TestGenerationEngineTLSAndHTTPPublishTogether(t *testing.T) {
	t.Setenv("TASK9_TLS_A_LOGIN", "login-a")
	t.Setenv("TASK9_TLS_B_LOGIN", "login-b")
	t.Setenv("TASK9_TLS_A_PASSWORD", "password-a")
	t.Setenv("TASK9_TLS_B_PASSWORD", "password-b")

	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("handler-a"))
	}))
	t.Cleanup(backendA.Close)
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("handler-b"))
	}))
	t.Cleanup(backendB.Close)
	fixture := newGenerationContractFixture(t, true)

	certA := generationContractSSL(t, "certificate-a", "certificate-a")
	prepareGenerationContract(t, fixture.engine, 221, generationContractResources(
		t, backendA.URL, "tls-a", "TASK9_TLS_A_LOGIN", "TASK9_TLS_A_PASSWORD", "", &certA,
	))
	ownerA := fixture.engine.active.Load().http
	assertHTTPAndTLSGeneration(t, fixture.engine, ownerA, "handler-a", "certificate-a")

	certB := generationContractSSL(t, "certificate-b", "certificate-b")
	prepareGenerationContract(t, fixture.engine, 222, generationContractResources(
		t, backendB.URL, "tls-b", "TASK9_TLS_B_LOGIN", "TASK9_TLS_B_PASSWORD", "", &certB,
	))
	ownerB := fixture.engine.active.Load().http
	if ownerB == nil || ownerB == ownerA || fixture.engine.active.Load().stream != nil {
		t.Fatalf("published HTTP/TLS bundle owner = %#v", fixture.engine.active.Load())
	}
	assertHTTPAndTLSGeneration(t, fixture.engine, ownerB, "handler-b", "certificate-b")
}

func newGenerationContractFixture(t *testing.T, frontendTLS bool) *generationContractFixture {
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
			Plugins: []string{"basic-auth", "file-logger", "grpc-transcode"},
		},
	}
	if frontendTLS {
		effective.Config.Apisix.Ssl = config.Ssl{
			Enable: true, Listen: []config.Listen{{Port: 9443}},
			SslProtocols: "TLSv1.2", SslCiphers: frontendTLS12Cipher,
		}
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest,
		effective,
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{
			Cluster: proxy.NopClusterObserver{}, Stream: func(streamruntime.Result) {},
		},
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
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("GenerationEngine.Close() error = %v", err)
		}
		if err := resolver.Close(context.Background()); err != nil {
			t.Errorf("GenerationSecretResolver.Close() error = %v", err)
		}
	})
	return &generationContractFixture{engine: engine, resolver: resolver}
}

func generationContractResources(
	t *testing.T,
	backendURL string,
	label string,
	loginEnv string,
	passwordEnv string,
	logPath string,
	sslResource *resource.SSL,
) []generation.Resource {
	t.Helper()
	parsed, err := url.Parse(backendURL)
	if err != nil {
		t.Fatal(err)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	routeResource := resource.Route{
		ID: "contract-route", Uri: "/",
		Upstream: resource.Upstream{
			Scheme: "http", Nodes: []resource.Node{{Host: host, Port: port, Weight: 1}},
		},
	}
	metadata := map[string]any{"path": "/tmp/" + label + ".log"}
	if logPath != "" {
		routeResource.Plugins = map[string]resource.PluginConfig{
			"basic-auth":  map[string]any{},
			"file-logger": map[string]any{},
			"grpc-transcode": map[string]any{
				"proto_id": "contract-proto", "service": "contract.Echo", "method": "Say",
			},
		}
		metadata = map[string]any{
			"path": logPath,
			"log_format": map[string]any{
				"generation": label + "-metadata", "consumer": "$consumer_name",
			},
		}
	}
	values := []struct {
		kind  string
		id    string
		value any
	}{
		{kind: "routes", id: "contract-route", value: routeResource},
		{kind: "consumers", id: label + "-consumer", value: resource.Consumer{
			Username: label + "-consumer",
			Plugins: map[string]resource.PluginConfig{
				"basic-auth": map[string]any{
					"username": "$ENV://" + loginEnv, "password": "$ENV://" + passwordEnv,
				},
			},
		}},
		{kind: "plugin_metadata", id: "file-logger", value: metadata},
		{kind: "protos", id: "contract-proto", value: resource.Proto{
			ID: "contract-proto", Content: generationContractProto(label),
		}},
	}
	if sslResource != nil {
		values = append(values, struct {
			kind  string
			id    string
			value any
		}{kind: "ssls", id: sslResource.ID, value: *sslResource})
	}
	resources := make([]generation.Resource, 0, len(values))
	for _, value := range values {
		raw, err := json.Marshal(value.value)
		if err != nil {
			t.Fatal(err)
		}
		resources = append(resources, generation.Resource{
			Key: generation.ResourceKey{Kind: value.kind, ID: value.id}, Value: raw,
		})
	}
	return resources
}

func prepareGenerationContract(
	t *testing.T,
	engine *GenerationEngine,
	revision uint64,
	resources []generation.Resource,
) generation.PublicationSet {
	t.Helper()
	desired, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   desired.Digest(),
		Cursor: generation.ProviderCursor{
			Provider: "generation-contract-test", Revision: fmt.Sprintf("%d", revision),
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set, err := engine.Publish(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return set
}

func generationContractGRPCBackend(observations chan<- generationRequestObservation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, password, _ := r.BasicAuth()
		frame, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		observations <- generationRequestObservation{
			consumerID: r.Header.Get("X-Consumer-Username"), consumerLogin: login, consumerSecret: password,
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", "0")
		_, _ = w.Write(frame)
	})
}

func generationContractRequest(label string) *http.Request {
	request := httptest.NewRequest(
		http.MethodGet,
		"http://contract.example/?"+label+"value="+label+"-value",
		nil,
	)
	request.SetBasicAuth(label+"-login", label+"-password")
	return request
}

func generationContractProto(label string) string {
	field := strings.ReplaceAll(label, "-", "_")
	return fmt.Sprintf(`syntax = "proto3";
package contract;
service Echo { rpc Say (Request) returns (Reply); }
message Request { string %svalue = 1; }
message Reply { string %sresult = 1; }`, field, field)
}

func receiveGenerationRequestObservation(
	t *testing.T,
	observations <-chan generationRequestObservation,
	name string,
) generationRequestObservation {
	t.Helper()
	select {
	case observation := <-observations:
		return observation
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		return generationRequestObservation{}
	}
}

func readGenerationContractLog(t *testing.T, filePath string) []byte {
	t.Helper()
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read generation log %s: %v", filePath, err)
	}
	return content
}

func assertGenerationObservation(t *testing.T, got, want generationRequestObservation) {
	t.Helper()
	if got.revision != want.revision || got.consumerID != want.consumerID ||
		got.consumerLogin != want.consumerLogin || got.consumerSecret != want.consumerSecret ||
		!bytes.Contains(got.metadata, want.metadata) || got.response != want.response {
		t.Fatalf("generation observation = %#v, want %#v", got, want)
	}
}

func generationContractSSL(t *testing.T, serverName, commonName string) resource.SSL {
	t.Helper()
	certificate := frontendHandshakeCertificate(
		t, commonName, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	)
	certificatePEM := make([]byte, 0)
	for _, der := range certificate.Certificate {
		certificatePEM = append(certificatePEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	privateKey, ok := certificate.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("private key type = %T, want *rsa.PrivateKey", certificate.PrivateKey)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	return resource.SSL{
		ID: serverName, Sni: serverName, Cert: string(certificatePEM), Key: string(keyPEM), Status: 1,
	}
}

func assertHTTPAndTLSGeneration(
	t *testing.T,
	engine *GenerationEngine,
	wantOwner *generationOwner,
	wantBody string,
	wantCertificate string,
) {
	t.Helper()
	active := engine.active.Load()
	if active == nil || active.http != wantOwner {
		t.Fatalf("active HTTP/TLS owner = %#v, want exact owner %p", active, wantOwner)
	}
	lease, ok := engine.acquireHTTP()
	if !ok || lease.Snapshot != wantOwner.prepared.HTTP() {
		t.Fatal("HTTP and TLS did not acquire from the exact active slot")
	}
	defer lease.Release()
	assertHTTPSnapshotHandlerAndCertificate(t, lease.Snapshot, wantBody, wantCertificate)
}

func assertHTTPSnapshotHandlerAndCertificate(
	t *testing.T,
	snapshot *compiler.HTTPSnapshot,
	wantBody string,
	wantCertificate string,
) {
	t.Helper()
	response := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := response.Body.String(); got != wantBody {
		t.Fatalf("generation handler body = %q, want %q", got, wantBody)
	}
	config := snapshot.TLSConfig()
	if config == nil || config.GetCertificate == nil {
		t.Fatal("generation TLS snapshot has no certificate selector")
	}
	certificate, err := config.GetCertificate(&tls.ClientHelloInfo{ServerName: wantCertificate})
	if err != nil {
		t.Fatalf("GetCertificate() error = %v", err)
	}
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Subject.CommonName != wantCertificate {
		t.Fatalf("generation certificate = %q, want %q", parsed.Subject.CommonName, wantCertificate)
	}
}

func receiveContractSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
