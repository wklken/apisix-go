package loggly

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type cancellationWatchConn struct {
	net.Conn
	closeCalls   atomic.Int32
	closeStarted chan struct{}
	releaseClose <-chan struct{}
}

func (c *cancellationWatchConn) Close() error {
	c.closeCalls.Add(1)
	select {
	case c.closeStarted <- struct{}{}:
	default:
	}
	if c.releaseClose != nil {
		<-c.releaseClose
	}
	return nil
}

func TestWatchConnectionCancellation(t *testing.T) {
	for _, scenario := range []string{
		"cancellation closes connection",
		"normal completion preserves reused connection",
		"cleanup joins close already running",
		"nil context is no-op",
	} {
		t.Run(scenario, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario != "nil context is no-op" {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			var releaseClose chan struct{}
			if scenario == "cleanup joins close already running" {
				releaseClose = make(chan struct{})
			}
			conn := &cancellationWatchConn{
				closeStarted: make(chan struct{}, 1),
				releaseClose: releaseClose,
			}
			cleanup := watchConnectionCancellation(ctx, conn)

			switch scenario {
			case "cancellation closes connection":
				cancel()
				cleanup()
			case "normal completion preserves reused connection":
				cleanup()
				cancel()
			case "cleanup joins close already running":
				cancel()
				select {
				case <-conn.closeStarted:
				case <-time.After(time.Second):
					t.Fatal("cancellation callback did not enter Close")
				}
				cleanupDone := make(chan struct{})
				go func() {
					cleanup()
					close(cleanupDone)
				}()
				select {
				case <-cleanupDone:
					t.Fatal("cleanup returned while Close was blocked")
				case <-time.After(20 * time.Millisecond):
				}
				close(releaseClose)
				select {
				case <-cleanupDone:
				case <-time.After(time.Second):
					t.Fatal("cleanup did not return after Close completed")
				}
			case "nil context is no-op":
				cleanup()
			}

			wantCloseCalls := int32(0)
			if scenario == "cancellation closes connection" ||
				scenario == "cleanup joins close already running" {
				wantCloseCalls = 1
			}
			if got := conn.closeCalls.Load(); got != wantCloseCalls {
				t.Fatalf("Close calls = %d, want %d", got, wantCloseCalls)
			}
		})
	}
}

type logglyScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type logglyScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []logglyScopedSecretCall
}

func (*logglyScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*logglyScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this Loggly fixture")
}

func (broker *logglyScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, logglyScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Loggly token test value")
	}
	return value, nil
}

func (*logglyScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *logglyScopedSecretBroker) callsSnapshot() []logglyScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]logglyScopedSecretCall(nil), broker.calls...)
}

func newLogglyScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *logglyScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: document,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "loggly-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	publication := generation.PublicationSet{
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
	broker := &logglyScopedSecretBroker{values: values, fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		_ = registration.Close(context.Background())
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close Loggly scoped attempt: %v", err)
		}
	}
}

func logglyTokenDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsOwnsLogglyToken(t *testing.T) {
	ciphertext := encryptLogglyTestValue(t, "qeddd145sfvddff3", "cipher-private")
	for index, tt := range []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "ciphertext", raw: ciphertext, resolved: "cipher-private"},
		{name: "environment", raw: "$ENV://LOGGLY_TOKEN", resolved: "env-private"},
		{name: "managed", raw: "$secret://vault/loggly/token", resolved: "managed-private"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{CustomerToken: tt.raw, Protocol: "http", Host: "http://127.0.0.1"}
			capabilityValue, scope, broker, closeAttempt := newLogglyScopedSecretHarness(
				t, uint64(index+1), "loggly-"+tt.name, config,
				map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.callsSnapshot()
			wantScope := scope
			wantScope.Field = "customer_token"
			if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != tt.raw {
				t.Fatalf("token calls = %#v, want scope %#v raw %q", calls, wantScope, tt.raw)
			}
			if p.config.CustomerToken != logglyTokenDescriptor(tt.resolved) {
				t.Fatalf("public token = %q, want descriptor", p.config.CustomerToken)
			}
			if p.httpClient != nil || p.BatchProcessor != nil {
				t.Fatal("materialization caused client or batch side effects")
			}
			if _, err := p.SendBatch(
				context.Background(), []map[string]any{{"premature": true}}, 1,
			); !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("pre-PostInit SendBatch() error = %v", err)
			}
			if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
				t.Fatalf("pre-PostInit RunLogPhase() error = %v", err)
			}
			p.Stop()
		})
	}

	const failedRaw = "$secret://vault/loggly/failure"
	config := Config{CustomerToken: failedRaw}
	capabilityValue, scope, broker, closeAttempt := newLogglyScopedSecretHarness(
		t, 10, "loggly-retry", config, map[string]string{failedRaw: "recovered-private"},
	)
	defer closeAttempt()
	broker.fail[failedRaw] = errors.New("resolver leaked " + failedRaw)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) || strings.Contains(err.Error(), failedRaw) {
		t.Fatalf("first materialization error = %v, want redacted unavailable", err)
	}
	if p.config.CustomerToken != failedRaw || p.secretsPrepared || p.tokenSet {
		t.Fatal("failed materialization installed partial state")
	}
	broker.mu.Lock()
	delete(broker.fail, failedRaw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if p.config.CustomerToken != logglyTokenDescriptor("recovered-private") {
		t.Fatalf("retry token = %q, want descriptor", p.config.CustomerToken)
	}
	p.Stop()
}

func TestLogglyScopedMaterializationIsSingleFlight(t *testing.T) {
	const raw = "$ENV://LOGGLY_SINGLEFLIGHT_TOKEN"
	config := Config{CustomerToken: raw}
	capabilityValue, scope, broker, closeAttempt := newLogglyScopedSecretHarness(
		t, 20, "loggly-singleflight", config, map[string]string{raw: "singleflight-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		})
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := broker.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("singleflight token calls = %#v, want one", calls)
	}
	p.Stop()
}

func TestLogglyTokensAreAttemptOwnedAcrossHTTPDeliveries(t *testing.T) {
	paths := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	newScoped := func(revision uint64, resourceID, raw, resolved string) (*Plugin, func()) {
		config := Config{
			CustomerToken: raw,
			Host:          server.URL,
			Protocol:      "http",
			Timeout:       1000,
			BatchMaxSize:  1,
		}
		capabilityValue, scope, _, closeAttempt := newLogglyScopedSecretHarness(
			t, revision, resourceID, config, map[string]string{raw: resolved},
		)
		p := &Plugin{config: config}
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
		if err := p.Init(); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		if err := p.PostInit(); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		processor := p.BatchProcessor
		return p, func() {
			p.Stop()
			if err := processor.Shutdown(context.Background()); err != nil {
				t.Errorf("batch Shutdown() error = %v", err)
			}
			closeAttempt()
		}
	}

	first, closeFirst := newScoped(31, "loggly-first", "$secret://loggly/first", "first-private")
	second, closeSecond := newScoped(32, "loggly-second", "$secret://loggly/second", "second-private")
	defer closeSecond()
	if first.config.CustomerToken == "first-private" || second.config.CustomerToken == "second-private" {
		t.Fatal("private token escaped into public config")
	}
	if _, err := first.SendBatch(context.Background(), []map[string]any{{"generation": 31}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-paths; got != "/bulk/first-private/tag/bulk" {
		t.Fatalf("first delivery path = %q", got)
	}
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": 32}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-paths; got != "/bulk/second-private/tag/bulk" {
		t.Fatalf("second delivery path = %q", got)
	}
	closeFirst()
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": "32-again"}}, 1); err != nil {
		t.Fatalf("second delivery after first Stop: %v", err)
	}
	if got := <-paths; got != "/bulk/second-private/tag/bulk" {
		t.Fatalf("second delivery after first Stop path = %q", got)
	}
}

type retainedLogglyRoundTripper struct {
	request  *http.Request
	response *http.Response
}

func (transport *retainedLogglyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	transport.response = &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("retained response")),
		Request:    request,
	}
	return transport.response, nil
}

func TestLogglyHTTPDeliveryScrubsRetainedTokenURLAndBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "retained-private",
		Host:          "http://loggly.invalid",
		Protocol:      "http",
		Timeout:       1000,
	})
	transport := &retainedLogglyRoundTripper{}
	p.httpClient = &http.Client{Transport: transport}
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"private": "request"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	if transport.request == nil || transport.response == nil {
		t.Fatal("round tripper did not retain request and response")
	}
	if transport.request.URL != nil {
		t.Fatalf("retained request URL = %#v, want nil", transport.request.URL)
	}
	if transport.request.Body != http.NoBody || transport.request.GetBody != nil {
		t.Fatalf(
			"retained request Body = %#v GetBody present = %t, want scrubbed",
			transport.request.Body, transport.request.GetBody != nil,
		)
	}
	if transport.response.Request != nil || transport.response.Body != http.NoBody {
		t.Fatalf("retained response still references request/body: %#v", transport.response)
	}
}

type blockingLogglyRoundTripper struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (transport *blockingLogglyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.once.Do(func() { close(transport.started) })
	<-transport.release
	return &http.Response{
		StatusCode: http.StatusAccepted,
		Header:     make(http.Header),
		Body:       http.NoBody,
		Request:    request,
	}, nil
}

func TestLogglyStopDrainsActiveSendAndPreventsResurrection(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "active-private",
		Host:          "http://loggly.invalid",
		Protocol:      "http",
		Timeout:       1000,
	})
	transport := &blockingLogglyRoundTripper{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	p.httpClient = &http.Client{Transport: transport}
	processor := p.BatchProcessor
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- err
	}()
	select {
	case <-transport.started:
	case <-time.After(time.Second):
		t.Fatal("active Loggly send did not start")
	}
	p.Stop()
	if p.httpClient == nil || p.BatchProcessor == nil || !p.tokenSet || !p.ready {
		t.Fatal("Stop cleaned active Loggly resources before delivery returned")
	}
	if !p.stopped.Load() {
		t.Fatal("Stop did not seal the Loggly log entrypoint")
	}
	close(transport.release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.httpClient != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatal("Stop retained client, processor, or private token")
	}
	before := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("post-Stop RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != before {
		t.Fatal("post-Stop log phase enqueued work")
	}
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"late": true}}, 1,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop PostInit() error = %v", err)
	}
}

func TestLogglyStopFlushesPendingBatchBeforeCleanup(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{
		CustomerToken:  "pending-private",
		Host:           server.URL,
		Protocol:       "http",
		Timeout:        1000,
		BatchMaxSize:   10,
		BufferDuration: 60,
	})
	if err := p.EnqueueLog(map[string]any{"pending": true}); err != nil {
		t.Fatal(err)
	}
	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	select {
	case path := <-received:
		if path != "/bulk/pending-private/tag/bulk" {
			t.Fatalf("pending delivery path = %q", path)
		}
	default:
		t.Fatal("Stop dropped the pending Loggly batch before delivery")
	}
	if p.httpClient != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatal("pending drain completed before runtime cleanup")
	}
}

func TestResolveLogglySnapshotFormatUsesRequestStartTime(t *testing.T) {
	started := time.Date(2026, time.August, 12, 8, 30, 0, 0, time.UTC)
	fields := resolveLogglySnapshotFormat(base.LogSnapshot{
		Started:  started,
		Finished: started.Add(5 * time.Second),
	}, map[string]string{"timestamp": "$time_iso8601"})
	if fields["timestamp"] != started.Format(time.RFC3339) {
		t.Fatalf("timestamp = %#v, want request start", fields["timestamp"])
	}
}

func TestSendBatchCancelsLogglyHTTPBulkWithContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Timeout:       10000,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(ctx, []map[string]any{{"path": "/cancel"}}, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Loggly bulk request")
	}
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("SendBatch() did not return after context cancellation")
	}
	if err == nil {
		t.Fatal("SendBatch() error = nil, want context cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendBatch() error = %v, want context cancellation when backend did not observe it", err)
		}
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	return newTestPluginWithMetadata(t, cfg, nil)
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata:       mustMetadataView(t, metadata),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newLogglyScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.CustomerToken: cfg.CustomerToken},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	if len(metadata) == 0 {
		return runtime.MetadataView{}
	}
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPostInitRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{config: Config{CustomerToken: "private"}}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets() error = %v, want credential unavailable", err)
	}
}

func TestPostInitSetsDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{CustomerToken: "token"})

	if p.config.Severity != "INFO" {
		t.Fatalf("severity = %q, want INFO", p.config.Severity)
	}
	if len(p.config.Tags) != 1 || p.config.Tags[0] != "apisix" {
		t.Fatalf("tags = %v, want [apisix]", p.config.Tags)
	}
	if p.config.Host != "logs-01.loggly.com" {
		t.Fatalf("host = %q, want logs-01.loggly.com", p.config.Host)
	}
	if p.config.Port != 514 {
		t.Fatalf("port = %d, want 514", p.config.Port)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
}

func TestPostInitRejectsInvalidEncryptedCustomerToken(t *testing.T) {
	p := &Plugin{config: Config{CustomerToken: "not-a-ciphertext"}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets() error = %v, want credential unavailable", err)
	}
}

func TestPostInitResolvesRotatedEncryptedCustomerToken(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{CustomerToken: encryptLogglyTestValue(t, oldKey, "loggly-token")}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newLogglyScopedSecretHarness(
		t, 1, "rotated-token", p.config, map[string]string{p.config.CustomerToken: "loggly-token"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.CustomerToken != logglyTokenDescriptor("loggly-token") {
		t.Fatalf("customer_token = %q, want resolved descriptor", p.config.CustomerToken)
	}
}

func TestBuildMessageUsesRFC5424ShapeAndTags(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		Tags:          []string{"apisix", "route-a"},
	})

	message := p.buildMessage(map[string]any{
		"status": 200,
		"path":   "/get",
	}, "token")

	if !strings.HasPrefix(message, "<14>1 ") {
		t.Fatalf("message = %q, want INFO priority prefix <14>1", message)
	}
	if !strings.Contains(message, `[token@41058 tag="apisix" tag="route-a"]`) {
		t.Fatalf("message = %q, want Loggly structured data with tags", message)
	}
	if !strings.Contains(message, `"path":"/get"`) {
		t.Fatalf("message = %q, want JSON log payload", message)
	}
}

func TestBuildMessageUsesSeverityMap(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		SeverityMap:   map[string]string{"503": "ERR"},
	})

	message := p.buildMessage(map[string]any{"status": 503}, "token")
	if !strings.HasPrefix(message, "<11>1 ") {
		t.Fatalf("message = %q, want ERR priority prefix <11>1", message)
	}
}

func TestHandlerBuildsDefaultAccessLogAndAddsRouteIDToCustomFormat(t *testing.T) {
	tests := []struct {
		name      string
		logFormat map[string]string
		assert    func(*testing.T, map[string]any)
	}{
		{
			name: "default access log",
			assert: func(t *testing.T, payload map[string]any) {
				t.Helper()
				request, ok := payload["request"].(map[string]any)
				if !ok || request["method"] != http.MethodGet || request["uri"] != "/orders?item=1" {
					t.Fatalf("request = %#v, want captured GET request", payload["request"])
				}
				response, ok := payload["response"].(map[string]any)
				if !ok || response["status"] != float64(http.StatusCreated) {
					t.Fatalf("response = %#v, want status 201", payload["response"])
				}
				if payload["route_id"] != "route-1" {
					t.Fatalf("route_id = %#v, want route-1", payload["route_id"])
				}
				if payload["client_ip"] == "" || payload["server"] == nil {
					t.Fatalf("payload = %#v, want client and server fields", payload)
				}
			},
		},
		{
			name:      "custom format",
			logFormat: map[string]string{"method": "$request_method"},
			assert: func(t *testing.T, payload map[string]any) {
				t.Helper()
				if payload["method"] != http.MethodGet {
					t.Fatalf("method = %#v, want GET", payload["method"])
				}
				if payload["route_id"] != "route-1" {
					t.Fatalf("route_id = %#v, want route-1", payload["route_id"])
				}
				if _, ok := payload["request"]; ok {
					t.Fatalf("payload = %#v, custom format must replace default fields", payload)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				received <- body
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				CustomerToken: "token",
				Host:          server.URL,
				Protocol:      "http",
				Timeout:       1000,
				BatchMaxSize:  1,
				LogFormat:     tt.logFormat,
			})
			p.RouteID = "route-1"
			p.ServerAddr = "127.0.0.1:8080"

			req := httptest.NewRequest(http.MethodGet, "http://localhost/orders?item=1", nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("created"))
			})).ServeHTTP(rr, req)

			select {
			case payload := <-received:
				tt.assert(t, payload)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for Loggly payload")
			}
		})
	}
}

func TestHandlerUsesRequestHostInRFC5424Envelope(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
		BatchMaxSize:  1,
		LogFormat:     map[string]string{"marker": "request-host"},
	})
	p.RouteID = "route-1"

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/orders", nil)
	req.Host = "127.0.0.1"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), req)

	select {
	case message := <-received:
		if !strings.Contains(message, " 127.0.0.1 apisix ") {
			t.Fatalf("message = %q, want request host in RFC5424 envelope", message)
		}
		if !strings.HasSuffix(message, ` {"marker":"request-host","route_id":"route-1"}`) {
			t.Fatalf("message = %q, want internal host field omitted from payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/get"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[token@41058 tag="apisix"]`) {
			t.Fatalf("message = %q, want Loggly token and default tag", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func TestSendWritesHTTPBulkMessage(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bulk/token/tag/bulk" {
			t.Fatalf("path = %q, want /bulk/token/tag/bulk", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-LOGGLY-TAG") != "apisix,route-a" {
			t.Fatalf("X-LOGGLY-TAG = %q, want apisix,route-a", r.Header.Get("X-LOGGLY-TAG"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body["path"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Tags:          []string{"apisix", "route-a"},
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/bulk"})

	select {
	case path := <-received:
		if path != "/bulk" {
			t.Fatalf("path = %q, want /bulk", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestHandlerBatchesHTTPBulkMessages(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Timeout:       1000,
		BatchMaxSize:  2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("bulk body = %q, want two newline-delimited entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("unmarshal bulk line %q: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loggly HTTP bulk body")
	}
}

func TestSendBatchWritesUDPMessagesIndividually(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	firstFail, err := p.SendBatch(context.Background(), []map[string]any{
		{"status": 200, "path": "/first"},
		{"status": 201, "path": "/second"},
	}, 2)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if firstFail != 0 {
		t.Fatalf("firstFail = %d, want 0", firstFail)
	}

	for _, want := range []string{"/first", "/second"} {
		select {
		case message := <-received:
			if !strings.Contains(message, want) {
				t.Fatalf("message = %q, want path %s", message, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for UDP log message %s", want)
		}
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:    "token",
		Host:             server.URL,
		Protocol:         "http",
		Timeout:          1000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("payload request body = %#v, want original request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("payload request body = %#v, want captured request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("payload response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want default request fields", payload["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("payload request = %#v, want no request body", request)
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want default response fields", payload["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("payload response = %#v, want no response body", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":         "token",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaMatchesAPISIX317SanityMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
		valid  bool
	}{
		{name: "full", config: map[string]any{
			"customer_token": "TEST-Token-Must-Be-Passed", "severity": "INFO",
			"tags": []any{"special-route", "highpriority-route"}, "max_retry_count": 0,
			"retry_delay": 1, "buffer_duration": 60, "inactive_timeout": 2, "batch_max_size": 10,
		}, valid: true},
		{name: "minimal", config: map[string]any{"customer_token": "minimized-config"}, valid: true},
		{name: "missing token", config: map[string]any{"severity": "DEBUG"}},
		{name: "unknown severity", config: map[string]any{"customer_token": "test", "severity": "UNKNOWN"}},
		{name: "lowercase severity", config: map[string]any{"customer_token": "test", "severity": "crit"}, valid: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.valid && err != nil {
				t.Fatalf("valid APISIX 3.17 configuration rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid APISIX 3.17 configuration accepted")
			}
		})
	}
}

func TestSchemaAcceptsOfficialBodySizeAndSSLFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
		"include_req_body":    true,
		"include_resp_body":   true,
		"ssl_verify":          false,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official config fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
		"batch_max_size":      2,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     60,
		"inactive_timeout":    5,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch and max pending fields: %v", err)
	}
}

func TestMetadataSchemaAcceptsEndpointAndLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"host":       "logs.example.com",
		"port":       -1,
		"protocol":   "custom",
		"timeout":    0,
		"log_format": map[string]any{"generation": "$route_id"},
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, metadata := range []map[string]any{
		{"host": 1},
		{"port": "514"},
		{"protocol": 1},
		{"timeout": "5000"},
		{"log_format": "$route_id"},
		{"log_format": map[string]any{"generation": 1}},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("invalid metadata accepted: %#v", metadata)
		}
	}
}

func TestPreparedGenerationsRetainMetadataEndpointAndFormat(t *testing.T) {
	first := newTestPluginWithMetadata(t, Config{CustomerToken: "token-n"}, map[string]any{
		"host":       "logs-n.example",
		"port":       1514,
		"protocol":   "http",
		"timeout":    1100,
		"log_format": map[string]any{"generation": "n"},
	})
	second := newTestPluginWithMetadata(t, Config{CustomerToken: "token-n-plus-one"}, map[string]any{
		"host":       "logs-n-plus-one.example",
		"port":       2514,
		"protocol":   "https",
		"timeout":    2100,
		"log_format": map[string]any{"generation": "n-plus-one"},
	})
	route := newTestPluginWithMetadata(t, Config{
		CustomerToken: "token-route",
		Host:          "logs-route.example",
		Port:          3514,
		Protocol:      "syslog",
		Timeout:       3100,
		LogFormat:     map[string]string{"generation": "route"},
	}, map[string]any{
		"host":       "logs-metadata.example",
		"port":       4514,
		"protocol":   "https",
		"timeout":    4100,
		"log_format": map[string]any{"generation": "metadata"},
	})

	if first.config.Host != "logs-n.example" || first.config.Port != 1514 ||
		first.config.Protocol != "http" || first.config.Timeout != 1100 ||
		first.LogFormat["generation"] != "n" {
		t.Fatalf("generation N metadata = %#v/%#v", first.config, first.LogFormat)
	}
	if second.config.Host != "logs-n-plus-one.example" || second.config.Port != 2514 ||
		second.config.Protocol != "https" || second.config.Timeout != 2100 ||
		second.LogFormat["generation"] != "n-plus-one" {
		t.Fatalf("generation N+1 metadata = %#v/%#v", second.config, second.LogFormat)
	}
	if route.config.Host != "logs-route.example" || route.config.Port != 3514 ||
		route.config.Protocol != "syslog" || route.config.Timeout != 3100 ||
		route.LogFormat["generation"] != "route" {
		t.Fatalf("route precedence = %#v/%#v", route.config, route.LogFormat)
	}
}

func TestMetadataDecodeFailsBeforeLogglyClientAndProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{CustomerToken: "token"}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string]any{
			"timeout": "invalid",
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newLogglyScopedSecretHarness(
		t, 1, "invalid-metadata", p.config, map[string]string{p.config.CustomerToken: p.config.CustomerToken},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.httpClient != nil || p.BatchProcessor != nil {
		t.Fatalf("decode failure acquired resources: client=%v processor=%v", p.httpClient, p.BatchProcessor)
	}
}

func encryptLogglyTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func startUDPServer(t *testing.T) (string, <-chan string) {
	return startUDPServerN(t, 1)
}

func startUDPServerN(t *testing.T, count int) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
	})

	received := make(chan string, count)
	go func() {
		buf := make([]byte, 4096)
		for range count {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

type countingHTTPListener struct {
	net.Listener
	accepts atomic.Int64
}

func (l *countingHTTPListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

func TestSendBatchReusesLogglyUDPSocket(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err != nil {
		t.Fatalf("SendBatch #2 error = %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1 reused socket", got)
	}
	for range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for loggly UDP message")
		}
	}
}

func TestSendBatchLogglyUDPUnblocksOnContextCancellation(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
	})
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          "unused",
		Port:          1,
		Timeout:       1000,
	})
	p.conn = client
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(ctx, []map[string]any{{"route_id": "blocked"}}, 1)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SendBatch() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendBatch() remained blocked after context cancellation")
	}
}

type cancelAfterWriteConn struct {
	cancel context.CancelFunc
	closed atomic.Bool
}

func (c *cancelAfterWriteConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *cancelAfterWriteConn) Write(payload []byte) (int, error) {
	c.cancel()
	return len(payload), nil
}

func (c *cancelAfterWriteConn) Close() error {
	c.closed.Store(true)
	return nil
}
func (*cancelAfterWriteConn) LocalAddr() net.Addr              { return nil }
func (*cancelAfterWriteConn) RemoteAddr() net.Addr             { return nil }
func (*cancelAfterWriteConn) SetDeadline(time.Time) error      { return nil }
func (*cancelAfterWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*cancelAfterWriteConn) SetWriteDeadline(time.Time) error { return nil }

func TestSendBatchLogglyUDPDiscardsConnectionCanceledAfterWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &cancelAfterWriteConn{cancel: cancel}
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          "unused",
		Port:          1,
		Timeout:       1000,
	})
	p.conn = conn
	_, err := p.SendBatch(ctx, []map[string]any{{"route_id": "deadline-race"}}, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SendBatch() error = %v, want context canceled", err)
	}
	if !conn.closed.Load() {
		t.Fatal("canceled connection was not closed")
	}
	p.connMu.Lock()
	retained := p.conn
	p.connMu.Unlock()
	if retained != nil {
		t.Fatal("canceled connection remained available for reuse")
	}
}

func TestSendBatchRedialsLogglyUDPSocketAfterFailure(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	var dials atomic.Int64
	p.dialFunc = func() (net.Conn, error) {
		dials.Add(1)
		return net.Dial("udp", addr)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	p.connMu.Lock()
	_ = p.conn.Close()
	p.connMu.Unlock()

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err == nil {
		t.Fatal("SendBatch #2 error = nil on a closed socket")
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r3"}}, 1); err != nil {
		t.Fatalf("SendBatch #3 error = %v, want redial delivery", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dial count after redial = %d, want 2", got)
	}
	for range 2 {
		select {
		case <-received:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for loggly UDP message")
		}
	}
}

func TestSendBatchReusesLogglyHTTPTransport(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	counting := &countingHTTPListener{Listener: ln}
	server.Listener = counting
	server.Start()
	t.Cleanup(server.Close)

	host := strings.TrimPrefix(server.URL, "http://")
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "http",
		Host:          host,
		Port:          80,
		Timeout:       1000,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err != nil {
		t.Fatalf("SendBatch #2 error = %v", err)
	}
	if got := counting.accepts.Load(); got != 1 {
		t.Fatalf("HTTP connections = %d, want 1 reused transport connection", got)
	}
}

func TestStopClosesLogglyUDPSocket(t *testing.T) {
	addr, received := startUDPServerN(t, 1)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Protocol:      "syslog",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})
	p.dialFunc = func() (net.Conn, error) {
		return net.Dial("udp", addr)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for loggly UDP message")
	}

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		t.Fatal("Stop() left the loggly UDP socket open")
	}
}
