package splunk_hec_logging

import (
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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type splunkScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

func TestSchemaMatchesAPISIX317Matrix(t *testing.T) {
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
			"endpoint": map[string]any{
				"uri": "http://127.0.0.1:18088/services/collector", "token": "token",
				"channel": "channel", "timeout": 1, "keepalive_timeout": 60000,
			},
			"max_retry_count": 0, "retry_delay": 1, "buffer_duration": 1,
			"inactive_timeout": 1, "batch_max_size": 1,
		}, valid: true},
		{name: "minimal", config: map[string]any{"endpoint": map[string]any{
			"uri": "http://127.0.0.1:18088/services/collector", "token": "token",
		}}, valid: true},
		{name: "missing uri", config: map[string]any{"endpoint": map[string]any{"token": "token"}}},
		{name: "missing token", config: map[string]any{"endpoint": map[string]any{
			"uri": "http://127.0.0.1:18088/services/collector",
		}}},
		{name: "invalid uri", config: map[string]any{"endpoint": map[string]any{
			"uri": "127.0.0.1:18088/services/collector", "token": "token",
		}}},
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

type splunkScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []splunkScopedSecretCall
}

func (*splunkScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*splunkScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this Splunk fixture")
}

func (broker *splunkScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, splunkScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Splunk token test value")
	}
	return value, nil
}

func (*splunkScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *splunkScopedSecretBroker) callsSnapshot() []splunkScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]splunkScopedSecretCall(nil), broker.calls...)
}

func newSplunkScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
	keyring ...string,
) (secret.GenerationCapability, secret.Scope, *splunkScopedSecretBroker, func()) {
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
			Key: key, Disposition: generation.DispositionPublished, Code: "splunk-test",
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
	broker := &splunkScopedSecretBroker{values: values, fail: make(map[string]error)}
	registration, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).RegisterCandidate(
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
			t.Errorf("close Splunk scoped attempt: %v", err)
		}
	}
}

func splunkTokenDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsOwnsSplunkToken(t *testing.T) {
	ciphertext := encryptSplunkTestValue(t, "qeddd145sfvddff3", "cipher-private")
	for index, tt := range []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "ciphertext", raw: ciphertext, resolved: "cipher-private"},
		{name: "environment", raw: "$ENV://SPLUNK_HEC_TOKEN", resolved: "env-private"},
		{name: "managed", raw: "$secret://vault/splunk/token", resolved: "managed-private"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{Endpoint: Endpoint{URI: "http://127.0.0.1:8088", Token: tt.raw}}
			capabilityValue, scope, broker, closeAttempt := newSplunkScopedSecretHarness(
				t, uint64(index+1), "splunk-"+tt.name, config,
				map[string]string{tt.raw: tt.resolved},
				"qeddd145sfvddff3",
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
			wantScope.Field = "endpoint.token"
			isReference := strings.HasPrefix(tt.raw, "$secret://") ||
				strings.HasPrefix(strings.ToUpper(tt.raw), "$ENV://")
			if !isReference && len(calls) != 0 {
				t.Fatalf("token calls = %#v, want none for ciphertext", calls)
			}
			if isReference && (len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != tt.raw) {
				t.Fatalf("token calls = %#v, want scope %#v raw %q", calls, wantScope, tt.raw)
			}
			if p.config.Endpoint.Token != splunkTokenDescriptor(tt.resolved) {
				t.Fatalf("public token = %q, want descriptor", p.config.Endpoint.Token)
			}
			if p.client != nil || p.BatchProcessor != nil {
				t.Fatal("materialization caused client or batch side effects")
			}
			p.Stop()
		})
	}

	const raw = "$secret://vault/splunk/retry"
	config := Config{Endpoint: Endpoint{URI: "http://127.0.0.1:8088", Token: raw}}
	capabilityValue, scope, broker, closeAttempt := newSplunkScopedSecretHarness(
		t, 10, "splunk-retry", config, map[string]string{raw: "recovered-private"},
	)
	defer closeAttempt()
	broker.fail[raw] = errors.New("resolver leaked " + raw)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) || strings.Contains(err.Error(), raw) {
		t.Fatalf("first materialization error = %v, want redacted unavailable", err)
	}
	if p.config.Endpoint.Token != raw || p.secretsPrepared || p.tokenSet {
		t.Fatal("failed materialization installed partial state")
	}
	broker.mu.Lock()
	delete(broker.fail, raw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if p.config.Endpoint.Token != splunkTokenDescriptor("recovered-private") {
		t.Fatalf("retry token = %q, want descriptor", p.config.Endpoint.Token)
	}
	p.Stop()
}

func TestSplunkScopedTokenMaterializationIsSingleFlight(t *testing.T) {
	const raw = "$ENV://SPLUNK_SINGLEFLIGHT_TOKEN"
	config := Config{Endpoint: Endpoint{URI: "http://127.0.0.1:8088", Token: raw}}
	capabilityValue, scope, broker, closeAttempt := newSplunkScopedSecretHarness(
		t, 20, "splunk-singleflight", config, map[string]string{raw: "singleflight-private"},
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

func TestSplunkGenerationsShareNeutralClientWithoutAuthorizationLeak(t *testing.T) {
	type requestAuth struct {
		authorization string
		channel       string
	}
	received := make(chan requestAuth, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- requestAuth{
			authorization: r.Header.Get("Authorization"),
			channel:       r.Header.Get("X-Splunk-Request-Channel"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	config := func(token string) Config {
		return Config{Endpoint: Endpoint{URI: server.URL, Token: token, Channel: "channel-a"}}
	}
	newScoped := func(revision uint64, resourceID, raw, resolved string) (*Plugin, func()) {
		pluginConfig := config(raw)
		capabilityValue, scope, _, closeAttempt := newSplunkScopedSecretHarness(
			t, revision, resourceID, pluginConfig, map[string]string{raw: resolved},
		)
		p := &Plugin{config: pluginConfig}
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
	first, closeFirst := newScoped(31, "splunk-first", "$secret://splunk/first", "first-private")
	second, closeSecond := newScoped(32, "splunk-second", "$secret://splunk/second", "second-private")
	defer closeSecond()
	if first.client != second.client {
		t.Fatal("equal neutral Splunk configuration did not share a client")
	}
	if got := first.client.Header.Get("Authorization"); got != "" {
		t.Fatalf("shared client retained Authorization = %q", got)
	}
	for _, tt := range []struct {
		plugin *Plugin
		want   string
	}{
		{plugin: first, want: "Splunk first-private"},
		{plugin: second, want: "Splunk second-private"},
	} {
		if _, err := tt.plugin.SendBatch(
			context.Background(), []map[string]any{{"generation": tt.want}}, 1,
		); err != nil {
			t.Fatal(err)
		}
		got := <-received
		if got.authorization != tt.want || got.channel != "channel-a" {
			t.Fatalf("request headers = %#v, want auth %q and channel", got, tt.want)
		}
	}
	closeFirst()
	if _, err := second.SendBatch(
		context.Background(), []map[string]any{{"generation": "second-again"}}, 1,
	); err != nil {
		t.Fatalf("second generation after first Stop: %v", err)
	}
	if got := <-received; got.authorization != "Splunk second-private" {
		t.Fatalf("second Authorization after first Stop = %#v", got)
	}
}

func TestSplunkSendBatchScrubsRetainedRequestState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Splunk retained-private" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Authorization", "Splunk response-private")
		_, _ = w.Write([]byte(`{"text":"private response"}`))
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
	p := newTestPlugin(t, Config{Endpoint: Endpoint{URI: server.URL, Token: "retained-private"}})
	p.client = client
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"private": "request"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	if retainedRequest == nil || retainedResponse == nil || retainedRawResponse == nil {
		t.Fatal("Resty hook did not retain request/response state")
	}
	if got := retainedRequest.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained request Authorization = %q", got)
	}
	if retainedRequest.Body != nil || retainedRequest.RawRequest == nil ||
		retainedRequest.RawRequest.Body != http.NoBody || retainedRequest.RawRequest.GetBody != nil {
		t.Fatalf("retained request body state = %#v", retainedRequest)
	}
	if got := retainedRequest.RawRequest.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained raw request Authorization = %q", got)
	}
	if retainedResponse.Request != nil || retainedResponse.RawResponse != nil || len(retainedResponse.Body()) != 0 {
		t.Fatalf("retained response still references request/raw/body: %#v", retainedResponse)
	}
	if got := retainedRawResponse.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained raw response Authorization = %q", got)
	}
	if retainedRawResponse.Request != nil || retainedRawResponse.Body != http.NoBody {
		t.Fatalf("retained raw response still owns request/body: %#v", retainedRawResponse)
	}
}

func TestSplunkRejectsPrePostInitLogEnqueue(t *testing.T) {
	p := &Plugin{config: Config{Endpoint: Endpoint{URI: "http://127.0.0.1:8088", Token: "unready"}}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	capabilityValue, scope, _, cleanup := newSplunkScopedSecretHarness(
		t, 1, "pre-post-init", p.config, map[string]string{p.config.Endpoint.Token: p.config.Endpoint.Token},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	before := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pre-PostInit RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != before {
		t.Fatal("pre-PostInit log phase enqueued into FireChan")
	}
	p.Stop()
}

func TestSplunkStopFlushesPendingBatchBeforeCleanup(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{
		Endpoint:       Endpoint{URI: server.URL, Token: "pending-private"},
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
	case authorization := <-received:
		if authorization != "Splunk pending-private" {
			t.Fatalf("pending Authorization = %q", authorization)
		}
	default:
		t.Fatal("Stop dropped pending Splunk batch")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatalf("pending drain completed before cleanup: %#v", p)
	}
}

func TestSplunkStopDrainsActiveSendAndPreventsResurrection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{Endpoint: Endpoint{URI: server.URL, Token: "active-private"}})
	processor := p.BatchProcessor
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("active Splunk send did not start")
	}
	p.Stop()
	if p.client == nil || p.clientRelease == nil || p.BatchProcessor == nil || !p.tokenSet || !p.ready {
		t.Fatal("Stop cleaned active Splunk resources before delivery returned")
	}
	if !p.stopped.Load() {
		t.Fatal("Stop did not seal the Splunk log entrypoint")
	}
	close(release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.tokenSet || p.secretsPrepared || p.ready {
		t.Fatalf("private/runtime state survived Stop: %#v", p)
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
}

func TestRunLogPhasePreservesSplunkDefaultEventFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{}
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
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
			Method: http.MethodGet, URI: "/orders?x=1", URL: "/orders?x=1", Scheme: "http",
			Host: "gateway.example", RemoteAddr: "192.0.2.3:443",
			APISIXVars: map[string]any{"$balancer_ip": "10.0.0.1", "$balancer_port": "8080"},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusAccepted, Bytes: 12},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		if fields["request_method"] != http.MethodGet || fields["response_status"] != http.StatusAccepted {
			t.Fatalf("default event fields = %#v", fields)
		}
		if fields["upstream"] != "10.0.0.1:8080" {
			t.Fatalf("upstream = %#v", fields["upstream"])
		}
		if fields["request_url"] != "http://gateway.example/orders?x=1" {
			t.Fatalf("request_url = %#v", fields["request_url"])
		}
	case <-time.After(time.Second):
		t.Fatal("detached Splunk entry was not delivered")
	}
}

func TestDefaultEventsRedactSensitiveHeaders(t *testing.T) {
	requestHeaders := http.Header{
		"Authorization": {"Bearer request-secret"},
		"Cookie":        {"session=request-secret"},
		"X-Trace-ID":    {"trace-a"},
	}
	responseHeaders := http.Header{
		"Set-Cookie": {"session=response-secret"},
		"X-Upstream": {"orders"},
	}

	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet,
			URI:    "/orders",
			URL:    "/orders",
			Scheme: "http",
			Host:   "gateway.example",
			Header: requestHeaders,
		},
		Response: apisixlog.ResponseLogSnapshot{Header: responseHeaders},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusOK},
	}
	assertSplunkHeadersSanitized(t, splunkSnapshotDefaultEvent(snapshot))

	request := httptest.NewRequest(http.MethodGet, "http://gateway.example/orders", nil)
	request.Header = requestHeaders.Clone()
	assertSplunkHeadersSanitized(t, buildDefaultEvent(
		captureRequest(request),
		responseHeaders,
		request,
		httpsnoop.Metrics{Code: http.StatusOK},
	))
}

func assertSplunkHeadersSanitized(t *testing.T, fields map[string]any) {
	t.Helper()
	requestHeaders := splunkTestHeaderMap(t, fields["request_headers"])
	if _, ok := requestHeaders["authorization"]; ok {
		t.Fatalf("request headers contain authorization: %#v", requestHeaders)
	}
	if _, ok := requestHeaders["cookie"]; ok {
		t.Fatalf("request headers contain cookie: %#v", requestHeaders)
	}
	if got := requestHeaders["x-trace-id"]; got != "trace-a" {
		t.Fatalf("request x-trace-id = %#v, want trace-a", got)
	}

	responseHeaders := splunkTestHeaderMap(t, fields["response_headers"])
	if _, ok := responseHeaders["set-cookie"]; ok {
		t.Fatalf("response headers contain set-cookie: %#v", responseHeaders)
	}
	if got := responseHeaders["x-upstream"]; got != "orders" {
		t.Fatalf("response x-upstream = %#v, want orders", got)
	}
}

func splunkTestHeaderMap(t *testing.T, value any) map[string]any {
	t.Helper()
	switch headers := value.(type) {
	case map[string]any:
		return headers
	case http.Header:
		result := make(map[string]any, len(headers))
		for name, values := range headers {
			key := strings.ToLower(name)
			if len(values) == 1 {
				result[key] = values[0]
			} else {
				result[key] = append([]string(nil), values...)
			}
		}
		return result
	default:
		t.Fatalf("header payload = %#v, want map or http.Header", value)
		return nil
	}
}

func TestLogCapturePolicyIncludesExtraBodyFields(t *testing.T) {
	p := &Plugin{
		BaseLoggerPlugin: base.BaseLoggerPlugin{RequestBodyBytes: 17, ResponseBodyBytes: 23},
		logFormatExtra:   map[string]string{"request": "$request_body", "response": "$response_body"},
	}
	policy := p.LogCapturePolicy()
	if policy.RequestBodyBytes != 17 || policy.ResponseBodyBytes != 23 {
		t.Fatalf("policy = %#v, want request=17 response=23", policy)
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
	capabilityValue, scope, _, cleanup := newSplunkScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.Endpoint.Token: cfg.Endpoint.Token},
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

func TestPostInitSetsSplunkDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	if p.config.Endpoint.Timeout != 10 {
		t.Fatalf("timeout = %d, want 10", p.config.Endpoint.Timeout)
	}
	if p.config.Endpoint.KeepaliveTimeout != 60000 {
		t.Fatalf("keepalive timeout = %d, want 60000", p.config.Endpoint.KeepaliveTimeout)
	}
	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestPostInitResolvesRotatedEncryptedToken(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{Endpoint: Endpoint{Token: encryptSplunkTestValue(t, oldKey, "splunk-token")}}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newSplunkScopedSecretHarness(
		t, 1, "rotated-token", p.config, map[string]string{p.config.Endpoint.Token: "splunk-token"},
		newKey, oldKey,
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.Endpoint.Token != splunkTokenDescriptor("splunk-token") {
		t.Fatalf("endpoint.token = %q, want resolved descriptor", p.config.Endpoint.Token)
	}
}

func TestBuildEventUsesSplunkHECShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	event := p.buildEvent(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if event.Source != "apache-apisix-splunk-hec-logging" {
		t.Fatalf("source = %q, want apache-apisix-splunk-hec-logging", event.Source)
	}
	if event.SourceType != "_json" {
		t.Fatalf("sourcetype = %q, want _json", event.SourceType)
	}
	if event.Host == "" {
		t.Fatal("host is empty")
	}
	if event.Event["path"] != "/orders" {
		t.Fatalf("event path = %v, want /orders", event.Event["path"])
	}
	if event.Event["status"] != 201 {
		t.Fatalf("event status = %v, want 201", event.Event["status"])
	}
	if event.Time <= 0 {
		t.Fatalf("event time = %v, want positive Unix timestamp", event.Time)
	}
}

func TestMetadataSchemaAcceptsAdditiveLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	metadata := map[string]any{
		"log_format_extra":    map[string]any{"upstream_host": "$upstream_unresolved_host"},
		"max_pending_entries": 1,
	}
	if err := util.Validate(metadata, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected additive log format: %v", err)
	}
	if err := util.Validate(map[string]any{"log_format": "wrong-type"}, p.GetMetadataSchema()); err == nil {
		t.Fatal("metadata schema accepted string log_format")
	}
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	endpoint := Endpoint{
		URI:   "http://127.0.0.1:8088/services/collector/event",
		Token: "token",
	}
	first := newTestPluginWithMetadata(t, Config{Endpoint: endpoint}, map[string]any{
		"log_format_extra":    map[string]any{"generation": "n-extra"},
		"max_pending_entries": 11,
	})
	second := newTestPluginWithMetadata(t, Config{Endpoint: endpoint}, map[string]any{
		"log_format":          map[string]any{"generation": "n-plus-one"},
		"log_format_extra":    map[string]any{"generation": "must-be-suppressed"},
		"max_pending_entries": 12,
	})
	route := newTestPluginWithMetadata(t, Config{
		Endpoint:  endpoint,
		LogFormat: map[string]string{"generation": "route"},
	}, map[string]any{
		"log_format_extra": map[string]any{"generation": "must-be-suppressed"},
	})

	if first.LogFormat != nil || first.logFormatExtra["generation"] != "n-extra" ||
		first.config.MaxPendingEntries != 11 {
		t.Fatalf(
			"generation N metadata = format=%#v extra=%#v pending=%d",
			first.LogFormat,
			first.logFormatExtra,
			first.config.MaxPendingEntries,
		)
	}
	if second.LogFormat["generation"] != "n-plus-one" || second.logFormatExtra != nil ||
		second.config.MaxPendingEntries != 12 {
		t.Fatalf(
			"generation N+1 metadata = format=%#v extra=%#v pending=%d",
			second.LogFormat,
			second.logFormatExtra,
			second.config.MaxPendingEntries,
		)
	}
	if route.LogFormat["generation"] != "route" || route.logFormatExtra != nil {
		t.Fatalf("route format did not suppress extras: format=%#v extra=%#v", route.LogFormat, route.logFormatExtra)
	}
}

func TestMetadataDecodeFailsBeforeSplunkClientAndProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{Endpoint: Endpoint{
		URI:   "http://127.0.0.1:8088/services/collector/event",
		Token: "token",
	}}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string]any{
			"max_pending_entries": "invalid",
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newSplunkScopedSecretHarness(
		t, 1, "invalid-metadata", p.config, map[string]string{p.config.Endpoint.Token: p.config.Endpoint.Token},
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
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"decode failure acquired resources: client=%v release=%v processor=%v",
			p.client,
			p.clientRelease != nil,
			p.BatchProcessor,
		)
	}
}

func TestHandlerBuildsDefaultEventAndDoesNotClobberFieldsWithExtraFormat(t *testing.T) {
	p := &Plugin{
		logFormatExtra: map[string]string{
			"response_status": "extra-must-not-clobber",
			"upstream_host":   "$upstream_unresolved_host",
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.ready = true

	req := httptest.NewRequest(
		http.MethodPost,
		"http://gateway.example:9443/orders?sku=one",
		strings.NewReader("payload"),
	)
	req.RemoteAddr = "192.0.2.44:4567"
	req.Header.Set("X-Request-Marker", "request-value")
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)

	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		apisixctx.RegisterApisixVar(r, "$balancer_port", "9080")
		w.Header().Set("X-Upstream-Marker", "response-value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	if got := entry["request_url"]; got != "http://gateway.example:9443/orders?sku=one" {
		t.Fatalf("request_url = %#v", got)
	}
	if got := entry["request_method"]; got != http.MethodPost {
		t.Fatalf("request_method = %#v", got)
	}
	requestHeaders := splunkTestHeaderMap(t, entry["request_headers"])
	if requestHeaders["x-request-marker"] != "request-value" {
		t.Fatalf("request_headers = %#v", entry["request_headers"])
	}
	requestQuery, ok := entry["request_query"].(map[string][]string)
	if !ok || len(requestQuery["sku"]) != 1 || requestQuery["sku"][0] != "one" {
		t.Fatalf("request_query = %#v", entry["request_query"])
	}
	if got := entry["request_size"]; got != int64(len("payload")) {
		t.Fatalf("request_size = %#v", got)
	}
	responseHeaders := splunkTestHeaderMap(t, entry["response_headers"])
	if responseHeaders["x-upstream-marker"] != "response-value" {
		t.Fatalf("response_headers = %#v", entry["response_headers"])
	}
	if got := entry["response_status"]; got != http.StatusCreated {
		t.Fatalf("response_status = %#v, want %d", got, http.StatusCreated)
	}
	if got := entry["response_size"]; got != int64(len("ok")) {
		t.Fatalf("response_size = %#v", got)
	}
	if got := entry["upstream"]; got != "10.0.0.8:9080" {
		t.Fatalf("upstream = %#v", got)
	}
	if got := entry["upstream_host"]; got != "10.0.0.8" {
		t.Fatalf("upstream_host = %#v", got)
	}
	if latency, ok := entry["latency"].(int64); !ok || latency < 0 {
		t.Fatalf("latency = %#v", entry["latency"])
	}
}

func TestHandlerTreatsExplicitEmptyLogFormatAsCustomAndSuppressesExtras(t *testing.T) {
	p := &Plugin{logFormatExtra: map[string]string{"upstream_host": "$upstream_unresolved_host"}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.LogFormat = map[string]string{}
	p.ready = true

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example:9443/empty", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		w.WriteHeader(http.StatusNoContent)
	}))

	if len(entry) != 0 {
		t.Fatalf("entry = %#v, want explicit empty custom event", entry)
	}
}

func TestHandlerResolvesCustomVariablesAfterUpstreamWithoutPorts(t *testing.T) {
	p := &Plugin{
		logFormatExtra: map[string]string{"ignored_extra": "must-not-appear"},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.ready = true
	p.LogFormat = map[string]string{
		"client_ip":     "$remote_addr",
		"host":          "$host",
		"upstream_host": "$upstream_unresolved_host",
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example:9443/delayed", nil)
	req.RemoteAddr = "192.0.2.44:4567"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	entry := captureHandlerEntry(t, p, req, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		r.Host = "upstream.internal:9080"
		w.WriteHeader(http.StatusNoContent)
	}))

	want := map[string]any{
		"client_ip":     "192.0.2.44",
		"host":          "gateway.example",
		"upstream_host": "10.0.0.8",
	}
	if len(entry) != len(want) {
		t.Fatalf("entry = %#v, want %#v", entry, want)
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("entry[%q] = %#v, want %#v", key, got, expected)
		}
	}
}

func captureHandlerEntry(
	t *testing.T,
	p *Plugin,
	req *http.Request,
	next http.Handler,
) map[string]any {
	t.Helper()

	p.Handler(next).ServeHTTP(httptest.NewRecorder(), req)
	select {
	case entry := <-p.FireChan:
		return entry
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk handler entry")
		return nil
	}
}

func TestSendPostsSplunkHECEvent(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:     server.URL,
			Token:   "secret-token",
			Channel: "channel-a",
			Timeout: 1,
		},
		SSLVerify: &sslVerify,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Splunk secret-token" {
			t.Fatalf("Authorization = %q, want Splunk secret-token", got)
		}
		if got := req.Header.Get("X-Splunk-Request-Channel"); got != "channel-a" {
			t.Fatalf("X-Splunk-Request-Channel = %q, want channel-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC request")
	}

	select {
	case body := <-bodies:
		event, ok := body["event"].(map[string]any)
		if !ok {
			t.Fatalf("body event = %#v, want object", body["event"])
		}
		if event["path"] != "/orders" {
			t.Fatalf("event path = %v, want /orders", event["path"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC body")
	}
}

func TestSendBatchPostsConcatenatedSplunkHECEvents(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   server.URL,
			Token: "secret-token",
		},
		BatchMaxSize: 2,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-bodies:
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("body = %q, want both Splunk events", body)
		}
		if strings.Contains(body, "\n") {
			t.Fatalf("body = %q, want APISIX 3.17 concatenated events without delimiters", body)
		}
		decoder := json.NewDecoder(strings.NewReader(body))
		for index := range 2 {
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				t.Fatalf("decode event %d: %v", index, err)
			}
		}
		var extra map[string]any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			t.Fatalf("decode trailing event = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC batch request")
	}
}

func encryptSplunkTestValue(t *testing.T, key string, value string) string {
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
