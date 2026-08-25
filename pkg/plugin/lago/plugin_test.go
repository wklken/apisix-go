package lago

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type lagoScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type lagoScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []lagoScopedSecretCall
}

func (*lagoScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*lagoScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by the Lago scoped fixture")
}

func (broker *lagoScopedSecretBroker) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, lagoScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Lago test token")
	}
	return value, nil
}

func (*lagoScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *lagoScopedSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
		return
	}
	broker.fail[raw] = err
}

func (broker *lagoScopedSecretBroker) callsSnapshot() []lagoScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]lagoScopedSecretCall(nil), broker.calls...)
}

func newLagoScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *lagoScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(
		revision, []generation.Resource{{Key: key, Value: document}}, nil,
	)
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
			Key: key, Disposition: generation.DispositionPublished, Code: "lago-scoped-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, DesiredDigest: snapshot.Digest(),
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
	broker := &lagoScopedSecretBroker{
		values: values,
		fail:   make(map[string]error),
	}
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
		Generation: revision, Attempt: registration.AttemptID(), Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close Lago scoped attempt: %v", err)
		}
	}
}

func lagoTokenDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return "plugin_config#sha256:" + hex.EncodeToString(digest[:])
}

func TestMaterializeScopedSecretsOwnsLagoToken(t *testing.T) {
	contextual := encryptLagoTestValue(t, "0123456789abcdef", "contextual-private")
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "contextual ciphertext", raw: contextual, resolved: "contextual-private"},
		{name: "environment", raw: "$ENV://LAGO_TOKEN", resolved: "environment-private"},
		{name: "managed", raw: "$secret://vault/lago/token", resolved: "managed-private"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := lagoTestConfig("http://127.0.0.1:3000", test.raw)
			capabilityValue, scope, broker, closeAttempt := newLagoScopedSecretHarness(
				t, uint64(70+index), "lago-raw", config, map[string]string{test.raw: test.resolved},
			)
			defer closeAttempt()
			p := newRawLagoPlugin(t, config)
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.callsSnapshot()
			wantScope := scope
			wantScope.Field = "token"
			if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != test.raw {
				t.Fatalf("resolver calls = %#v, want exact token scope %#v", calls, wantScope)
			}
			if p.config.Token != lagoTokenDescriptor(test.resolved) ||
				p.config.Token == test.raw || p.config.Token == test.resolved {
				t.Fatalf("public token = %q, want resolved descriptor", p.config.Token)
			}
			if p.client != nil || p.BatchProcessor != nil {
				t.Fatal("materialization created client or processor before PostInit")
			}
			p.Stop()
		})
	}

	const raw = "$secret://vault/lago/retry"
	config := lagoTestConfig("http://127.0.0.1:3000", raw)
	capabilityValue, scope, broker, closeAttempt := newLagoScopedSecretHarness(
		t, 80, "lago-retry", config, map[string]string{raw: "retry-private"},
	)
	defer closeAttempt()
	broker.setFailure(raw, errors.New("resolver leaked "+raw+" retry-private"))
	p := newRawLagoPlugin(t, config)
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "retry-private") {
		t.Fatalf("failed materialization error = %v, want redacted", err)
	}
	if p.config.Token != raw || p.tokenSet || p.secretsPrepared || p.client != nil || p.BatchProcessor != nil {
		t.Fatalf("failed materialization installed partial state: %#v", p)
	}
	broker.setFailure(raw, nil)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if p.config.Token != lagoTokenDescriptor("retry-private") {
		t.Fatalf("retry public token = %q", p.config.Token)
	}

	concurrentConfig := lagoTestConfig("http://127.0.0.1:3000", "$ENV://LAGO_SINGLEFLIGHT")
	concurrentCapability, concurrentScope, concurrentBroker, closeConcurrent := newLagoScopedSecretHarness(
		t, 81, "lago-singleflight", concurrentConfig,
		map[string]string{concurrentConfig.Token: "singleflight-private"},
	)
	defer closeConcurrent()
	concurrent := newRawLagoPlugin(t, concurrentConfig)
	start := make(chan struct{})
	errs := make(chan error, 12)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), concurrentScope, concurrentCapability, concurrent,
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
	if calls := concurrentBroker.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("singleflight calls = %#v, want one", calls)
	}
	concurrent.Stop()
}

func TestSendBatchUsesPrivateTokenAndScrubsRetainedRequestState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer retained-private" {
			t.Fatalf("Authorization = %q, want private token", got)
		}
		w.Header().Set("Authorization", "Bearer response-private")
		_, _ = w.Write([]byte("private response"))
	}))
	t.Cleanup(server.Close)

	var retainedRequest *resty.Request
	var retainedResponse *resty.Response
	var retainedRawResponse *http.Response
	client := resty.New()
	client.OnAfterResponse(func(_ *resty.Client, response *resty.Response) error {
		retainedRequest = response.Request
		retainedResponse = response
		retainedRawResponse = response.RawResponse
		return nil
	})
	config := lagoTestConfig(server.URL, "retained-private")
	p := &Plugin{config: config}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	capabilityValue, scope, _, cleanup := newLagoScopedSecretHarness(
		t, 1, "lago-scrub", config, map[string]string{config.Token: config.Token},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.config.EndpointURI = "/api/v1/events/batch"
	p.config.Timeout = 1000
	p.client = client
	p.now = time.Now
	p.ready = true
	t.Cleanup(p.Stop)
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"request_id": "private-request"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	if retainedRequest == nil || retainedResponse == nil || retainedRawResponse == nil {
		t.Fatal("Resty hook did not retain request/response state")
	}
	if got := retainedRequest.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained request Authorization = %q", got)
	}
	if retainedRequest.Body != nil {
		t.Fatalf("retained request Body = %#v, want nil", retainedRequest.Body)
	}
	if retainedRequest.RawRequest == nil {
		t.Fatal("retained raw request unexpectedly nil")
	}
	if got := retainedRequest.RawRequest.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained raw request Authorization = %q", got)
	}
	if retainedRequest.RawRequest.Body != http.NoBody || retainedRequest.RawRequest.GetBody != nil {
		t.Fatalf(
			"retained raw request body = %#v GetBody present=%t, want scrubbed",
			retainedRequest.RawRequest.Body, retainedRequest.RawRequest.GetBody != nil,
		)
	}
	if retainedResponse.Request != nil || retainedResponse.RawResponse != nil || len(retainedResponse.Body()) != 0 {
		t.Fatalf("retained response still references request/raw/body: %#v", retainedResponse)
	}
	if got := retainedRawResponse.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained raw response Authorization = %q", got)
	}
	if retainedRawResponse.Body != http.NoBody || retainedRawResponse.Request != nil {
		t.Fatalf(
			"retained raw response Body = %#v Request present=%t, want detached",
			retainedRawResponse.Body, retainedRawResponse.Request != nil,
		)
	}
}

func TestLagoGenerationsShareNeutralClientWithoutAuthorizationLeak(t *testing.T) {
	authorizations := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	first := newTestPlugin(t, lagoTestConfig(server.URL, "first-private"))
	second := newTestPlugin(t, lagoTestConfig(server.URL, "second-private"))
	if first.client != second.client {
		t.Fatal("equal neutral HTTP configuration did not share a client")
	}
	if first.client.Header.Get("Authorization") != "" {
		t.Fatal("shared neutral client retained Authorization")
	}
	if _, err := first.SendBatch(context.Background(), []map[string]any{{"generation": 1}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-authorizations; got != "Bearer first-private" {
		t.Fatalf("first Authorization = %q", got)
	}
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": 2}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-authorizations; got != "Bearer second-private" {
		t.Fatalf("second Authorization = %q", got)
	}
	first.Stop()
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": "2-again"}}, 1); err != nil {
		t.Fatalf("second generation after first Stop: %v", err)
	}
	if got := <-authorizations; got != "Bearer second-private" {
		t.Fatalf("second Authorization after first Stop = %q", got)
	}
}

func TestLagoStopDrainsActiveSendAndPreventsResurrection(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseRequest:
		default:
			close(releaseRequest)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, lagoTestConfig(server.URL, "active-private"))
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("active Lago request did not start")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active Lago request drained")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseRequest)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after active Lago request drained")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatalf("private/runtime state survived Stop: %#v", p)
	}
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"late": true}}, 1,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	queued := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("post-Stop RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != queued {
		t.Fatal("post-Stop RunLogPhase enqueued work")
	}
	nextCalled := false
	handler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !nextCalled {
		t.Fatal("post-Stop handler did not call next")
	}
	if len(p.FireChan) != queued {
		t.Fatal("post-Stop handler enqueued work")
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop PostInit() error = %v", err)
	}
	p.Stop()
}

func TestLagoRejectsPrePostInitLogEnqueue(t *testing.T) {
	config := lagoTestConfig("http://127.0.0.1:3000", "unready-private")
	p := newRawLagoPlugin(t, config)
	capabilityValue, scope, _, cleanup := newLagoScopedSecretHarness(
		t, 1, "lago-unready", config, map[string]string{config.Token: config.Token},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	before := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pre-PostInit RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != before {
		t.Fatal("pre-PostInit log phase enqueued into the unowned FireChan")
	}
	p.Stop()
}

func TestLagoStopFlushesPendingBatchBeforeCleanup(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	config := lagoTestConfig(server.URL, "pending-private")
	config.BatchMaxSize = 10
	config.BufferDuration = 60
	p := newTestPlugin(t, config)
	if err := p.EnqueueLog(map[string]any{"pending": true}); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	select {
	case authorization := <-received:
		if authorization != "Bearer pending-private" {
			t.Fatalf("pending Authorization = %q", authorization)
		}
	default:
		t.Fatal("Stop dropped the pending Lago batch before delivery")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatalf("pending drain completed before runtime cleanup: %#v", p)
	}
}

func TestLagoConcurrentPostInitAndStopCannotPublishRuntime(t *testing.T) {
	config := lagoTestConfig("http://127.0.0.1:3000", "post-init-private")
	p := newRawLagoPlugin(t, config)
	capabilityValue, scope, _, cleanup := newLagoScopedSecretHarness(
		t, 1, "lago-concurrent", config, map[string]string{config.Token: config.Token},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	p.lifecycleMu.Lock()
	postInitDone := make(chan error, 1)
	go func() { postInitDone <- p.PostInit() }()
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	deadline := time.Now().Add(time.Second)
	for !p.stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !p.stopped.Load() {
		p.lifecycleMu.Unlock()
		t.Fatal("concurrent Stop did not retire plugin")
	}
	p.lifecycleMu.Unlock()

	if err := <-postInitDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("concurrent PostInit() error = %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop did not finish")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatalf("concurrent PostInit/Stop published runtime state: %#v", p)
	}
	p.Stop()
}

func lagoTestConfig(endpoint, token string) Config {
	return Config{
		EndpointAddrs: []string{endpoint}, Token: token,
		EventTransactionID: "req-1", EventSubscriptionID: "sub-1", EventCode: "api-call",
	}
}

func newRawLagoPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	endpointAddrs := make([]any, len(config.EndpointAddrs))
	for index, endpoint := range config.EndpointAddrs {
		endpointAddrs[index] = endpoint
	}
	rawConfig := map[string]any{
		"endpoint_addrs":        endpointAddrs,
		"token":                 config.Token,
		"event_transaction_id":  config.EventTransactionID,
		"event_subscription_id": config.EventSubscriptionID,
		"event_code":            config.EventCode,
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunLogPhasePreservesLagoTemplateFieldsAndBodies(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{config: Config{
		IncludeReqBody: true, IncludeRespBody: true, MaxReqBodyBytes: 64, MaxRespBodyBytes: 64,
		EventTransactionID: "${request_method}-${status}", EventProperties: map[string]string{"route": "${route_id}"},
	}}
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	p.ready = true
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodPost, URI: "/orders", Body: []byte("request-body"),
			APISIXVars: map[string]any{"$route_id": "route-1"},
		},
		Response: apisixlog.ResponseLogSnapshot{Body: []byte("response-body")},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusAccepted},
		Started:  time.Unix(10, 0), Finished: time.Unix(11, 0),
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		if fields["request_body"] != "request-body" || fields["response_body"] != "response-body" {
			t.Fatalf("body fields = %#v/%#v", fields["request_body"], fields["response_body"])
		}
		if fields[requestStartTimeField] == nil {
			t.Fatal("request start time was not preserved")
		}
	case <-time.After(time.Second):
		t.Fatal("detached Lago entry was not delivered")
	}
}

type capabilityResponseWriter struct {
	http.ResponseWriter
	conn    net.Conn
	flushed bool
}

func (w *capabilityResponseWriter) Flush() {
	w.flushed = true
}

func (w *capabilityResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestResponseRecorderExposesResponseWriterCapabilities(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })

	underlying := &capabilityResponseWriter{conn: server}
	recorder := &responseRecorder{ResponseWriter: underlying}

	controller := http.NewResponseController(recorder)
	if err := controller.Flush(); err != nil {
		t.Fatalf("Flush() error = %v, want delegated flush", err)
	}
	if !underlying.flushed {
		t.Fatal("Flush() did not reach the underlying writer")
	}

	conn, rw, err := controller.Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v, want delegated hijack", err)
	}
	if conn == nil {
		t.Fatal("Hijack() returned nil connection")
	}
	if rw == nil {
		t.Fatal("Hijack() returned nil ReadWriter")
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newLagoScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.Token: cfg.Token},
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

func TestMaterializeSecretsRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{config: Config{Token: "private"}}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets() error = %v, want credential unavailable", err)
	}
}

func TestPostInitSetsLagoDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{"http://127.0.0.1:3000"},
		Token:               "token",
		EventTransactionID:  "req-1",
		EventSubscriptionID: "sub-1",
		EventCode:           "api-call",
	})

	if p.config.EndpointURI != "/api/v1/events/batch" {
		t.Fatalf("endpoint_uri = %q, want /api/v1/events/batch", p.config.EndpointURI)
	}
	if p.config.Timeout != 3000 {
		t.Fatalf("timeout = %d, want 3000", p.config.Timeout)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatal("ssl_verify = false, want true")
	}
	if !p.keepalive() {
		t.Fatal("keepalive() = false, want true by default")
	}
	if p.config.KeepaliveTimeout != 60000 {
		t.Fatalf("keepalive_timeout = %d, want 60000", p.config.KeepaliveTimeout)
	}
	if p.config.KeepalivePool != 5 {
		t.Fatalf("keepalive_pool = %d, want 5", p.config.KeepalivePool)
	}
	if p.config.BatchMaxSize != 100 {
		t.Fatalf("batch_max_size = %d, want 100", p.config.BatchMaxSize)
	}
}

func TestPostInitRejectsInvalidEncryptedToken(t *testing.T) {
	p := &Plugin{config: Config{Token: "not-a-ciphertext"}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets() error = %v, want credential unavailable", err)
	}
}

func TestPostInitResolvesRotatedEncryptedToken(t *testing.T) {
	oldKey := "old-keyring-item"
	config := Config{Token: encryptLagoTestValue(t, oldKey, "lago-token")}
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newLagoScopedSecretHarness(
		t, 1, "lago-rotated", config, map[string]string{config.Token: "lago-token"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.Token != lagoTokenDescriptor("lago-token") {
		t.Fatalf("token = %q, want resolved descriptor", p.config.Token)
	}
}

func TestBuildEventResolvesConfiguredTemplates(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{"http://127.0.0.1:3000"},
		Token:               "token",
		EventTransactionID:  "req_${request_id}",
		EventSubscriptionID: "sub_${consumer_name}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"status": "${status}",
			"tier":   "expensive",
		},
	})

	entry := p.buildEvent(map[string]any{
		"request_id":    "abc",
		"consumer_name": "alice",
		"status":        201,
	})

	if entry.TransactionID != "req_abc" {
		t.Fatalf("transaction_id = %q, want req_abc", entry.TransactionID)
	}
	if entry.ExternalSubscriptionID != "sub_alice" {
		t.Fatalf("external_subscription_id = %q, want sub_alice", entry.ExternalSubscriptionID)
	}
	if entry.Code != "api-call" {
		t.Fatalf("code = %q, want api-call", entry.Code)
	}
	if entry.Properties["status"] != "201" {
		t.Fatalf("status property = %q, want 201", entry.Properties["status"])
	}
	if entry.Properties["tier"] != "expensive" {
		t.Fatalf("tier property = %q, want expensive", entry.Properties["tier"])
	}
	if entry.Timestamp <= 0 {
		t.Fatalf("timestamp = %f, want positive Unix timestamp", entry.Timestamp)
	}
}

func TestEndpointURLSelectsFromEndpointAddrs(t *testing.T) {
	oldRandomEndpointIndex := randomEndpointIndex
	randomEndpointIndex = func(n int) int {
		if n != 2 {
			t.Fatalf("random endpoint count = %d, want 2", n)
		}
		return 1
	}
	t.Cleanup(func() {
		randomEndpointIndex = oldRandomEndpointIndex
	})

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{"http://127.0.0.1:3000", "http://127.0.0.2:3000"},
		Token:               "token",
		EventTransactionID:  "req-1",
		EventSubscriptionID: "sub-1",
		EventCode:           "api-call",
	})

	if got := p.endpointURL(); got != "http://127.0.0.2:3000/api/v1/events/batch" {
		t.Fatalf("endpointURL() = %q, want selected endpoint_addrs entry", got)
	}
}

func TestSendPostsLagoBatchEvent(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "lago-token",
		EventTransactionID:  "${request_id}",
		EventSubscriptionID: "${consumer_name}",
		EventCode:           "api-call",
		Timeout:             1000,
	})

	p.Send(map[string]any{
		"request_id":    "req-1",
		"consumer_name": "sub-1",
	})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/api/v1/events/batch" {
			t.Fatalf("path = %q, want /api/v1/events/batch", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer lago-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago request")
	}

	select {
	case body := <-bodies:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.TransactionID != "req-1" {
			t.Fatalf("transaction_id = %q, want req-1", event.TransactionID)
		}
		if event.ExternalSubscriptionID != "sub-1" {
			t.Fatalf("external_subscription_id = %q, want sub-1", event.ExternalSubscriptionID)
		}
		if event.Code != "api-call" {
			t.Fatalf("code = %q, want api-call", event.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago body")
	}
}

func TestSendBatchPostsMultipleLagoEvents(t *testing.T) {
	bodies := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "lago-token",
		EventTransactionID:  "${request_id}",
		EventSubscriptionID: "${consumer_name}",
		EventCode:           "api-call",
		Timeout:             1000,
		BatchMaxSize:        2,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{
		{"request_id": "req-1", "consumer_name": "sub-1"},
		{"request_id": "req-2", "consumer_name": "sub-2"},
	}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-bodies:
		if len(body.Events) != 2 {
			t.Fatalf("events = %d, want 2", len(body.Events))
		}
		if body.Events[0].TransactionID != "req-1" || body.Events[1].TransactionID != "req-2" {
			t.Fatalf("events = %#v, want both transaction IDs", body.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago batch request")
	}
}

func TestHandlerCapturesRequestAndResponseVariables(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "${http_x_request_id}",
		EventSubscriptionID: "${request_method}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"path":   "${uri}",
			"status": "${status}",
		},
		Timeout:      1000,
		BatchMaxSize: 1,
	})

	req := httptest.NewRequest(http.MethodPut, "/orders/1?debug=true", strings.NewReader("request"))
	req.Header.Set("X-Request-ID", "req-1")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	select {
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.TransactionID != "req-1" {
			t.Fatalf("transaction_id = %q, want req-1", event.TransactionID)
		}
		if event.ExternalSubscriptionID != http.MethodPut {
			t.Fatalf("external_subscription_id = %q, want PUT", event.ExternalSubscriptionID)
		}
		if event.Properties["path"] != "/orders/1" {
			t.Fatalf("path property = %q, want /orders/1", event.Properties["path"])
		}
		if event.Properties["status"] != "201" {
			t.Fatalf("status property = %q, want 201", event.Properties["status"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}

func TestHandlerResolvesDynamicRequestAndResponseVariables(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "${arg_request_id}",
		EventSubscriptionID: "${cookie_subscription}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"plan":       "${arg_plan}",
			"request_id": "${http_x_request_id}",
			"upstream":   "${sent_http_x_upstream_plan}",
		},
		Timeout:      1000,
		BatchMaxSize: 1,
	})

	req := httptest.NewRequest(http.MethodGet, "/orders?request_id=req-1&plan=pro", nil)
	req.Header.Set("Cookie", "subscription=sub-1")
	req.Header.Set("X-Request-ID", "header-req")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Plan", "enterprise")
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	select {
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.TransactionID != "req-1" {
			t.Fatalf("transaction_id = %q, want query arg request_id", event.TransactionID)
		}
		if event.ExternalSubscriptionID != "sub-1" {
			t.Fatalf("external_subscription_id = %q, want cookie subscription", event.ExternalSubscriptionID)
		}
		if event.Properties["plan"] != "pro" {
			t.Fatalf("plan property = %q, want query arg plan", event.Properties["plan"])
		}
		if event.Properties["request_id"] != "header-req" {
			t.Fatalf("request_id property = %q, want request header", event.Properties["request_id"])
		}
		if event.Properties["upstream"] != "enterprise" {
			t.Fatalf("upstream property = %q, want response header", event.Properties["upstream"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}

func TestHandlerCapturesRequestAndResponseBodies(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "${http_x_request_id}",
		EventSubscriptionID: "${request_method}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"request_body":  "${request_body}",
			"response_body": "${response_body}",
		},
		Timeout:          1000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":1}`))
	req.Header.Set("X-Request-ID", "req-1")
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
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.Properties["request_body"] != `{"order":1}` {
			t.Fatalf("request_body property = %q, want original request body", event.Properties["request_body"])
		}
		if event.Properties["response_body"] != `{"ok":true}` {
			t.Fatalf("response_body property = %q, want upstream response body", event.Properties["response_body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}

func TestHandlerUsesRequestStartTimeAsEventTimestamp(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "req-1",
		EventSubscriptionID: "sub-1",
		EventCode:           "api-call",
		Timeout:             1000,
		BatchMaxSize:        1,
	})
	requestStart := time.Unix(1710000000, 250000000)
	later := requestStart.Add(5 * time.Second)
	calls := 0
	p.now = func() time.Time {
		calls++
		if calls == 1 {
			return requestStart
		}
		return later
	}

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	select {
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		want := float64(requestStart.UnixNano()) / float64(time.Second)
		if math.Abs(body.Events[0].Timestamp-want) > 0.001 {
			t.Fatalf("timestamp = %f, want request start %f", body.Events[0].Timestamp, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}

func encryptLagoTestValue(t *testing.T, key string, value string) string {
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
