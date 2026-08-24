package http_logger

import (
	"bytes"
	"compress/gzip"
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

	brotli "github.com/andybalholm/brotli"
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

type httpLoggerScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type httpLoggerScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []httpLoggerScopedSecretCall
}

func (*httpLoggerScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*httpLoggerScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this HTTP logger fixture")
}

func (broker *httpLoggerScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, httpLoggerScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private HTTP authorization test value")
	}
	return value, nil
}

func (*httpLoggerScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *httpLoggerScopedSecretBroker) scopedCalls() []httpLoggerScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]httpLoggerScopedSecretCall(nil), broker.calls...)
}

func newHTTPLoggerScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *httpLoggerScopedSecretBroker, func()) {
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
			Key: key, Disposition: generation.DispositionPublished, Code: "http-logger-test",
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
	broker := &httpLoggerScopedSecretBroker{
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
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close HTTP logger scoped attempt: %v", err)
		}
	}
}

func httpAuthorizationDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func newRawHTTPLoggerPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMaterializeScopedSecretsOwnsHTTPAuthorization(t *testing.T) {
	for index, authHeader := range []*string{nil, new(string)} {
		name := "nil"
		if authHeader != nil {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			config := Config{URI: "http://127.0.0.1/logs", AuthHeader: authHeader}
			capabilityValue, scope, broker, closeAttempt := newHTTPLoggerScopedSecretHarness(
				t, uint64(index+1), "http-optional", config, nil,
			)
			defer closeAttempt()
			p := newRawHTTPLoggerPlugin(t, config)
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			if calls := broker.scopedCalls(); len(calls) != 0 {
				t.Fatalf("optional auth_header calls = %#v, want none", calls)
			}
			if err := p.PostInit(); err != nil {
				t.Fatalf("PostInit() without authorization error = %v", err)
			}
			t.Cleanup(p.Stop)
		})
	}

	ciphertext := encryptHTTPLoggerTestValue(t, "qeddd145sfvddff3", "Bearer ciphertext")
	for index, tt := range []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "literal", raw: "Bearer literal", resolved: "Bearer literal"},
		{name: "ciphertext", raw: ciphertext, resolved: "Bearer ciphertext"},
		{name: "environment", raw: "$ENV://HTTP_LOGGER_AUTH", resolved: "Bearer environment"},
		{name: "managed", raw: "$secret://vault/http-logger/auth", resolved: "Bearer managed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			receivedAuthorization := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedAuthorization <- r.Header.Get("Authorization")
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(server.Close)
			raw := tt.raw
			config := Config{URI: server.URL, AuthHeader: &raw, BatchMaxSize: 1}
			capabilityValue, scope, broker, closeAttempt := newHTTPLoggerScopedSecretHarness(
				t, uint64(10+index), "http-private", config,
				map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()
			p := newRawHTTPLoggerPlugin(t, config)
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.scopedCalls()
			wantScope := scope
			wantScope.Field = "auth_header"
			if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != tt.raw {
				t.Fatalf("auth_header calls = %#v, want scope %#v raw %q", calls, wantScope, tt.raw)
			}
			if p.config.AuthHeader == nil || *p.config.AuthHeader != httpAuthorizationDescriptor(tt.resolved) {
				t.Fatalf("public auth_header = %#v, want resolved descriptor", p.config.AuthHeader)
			}
			if p.client != nil || p.BatchProcessor != nil {
				t.Fatal("materialization caused client or batch side effects")
			}
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.Stop)
			if _, err := p.SendBatch(
				context.Background(), []map[string]any{{"path": "/scoped"}}, 1,
			); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-receivedAuthorization:
				if got != tt.resolved {
					t.Fatalf("Authorization = %q, want %q", got, tt.resolved)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for scoped HTTP logger request")
			}
		})
	}

	const blankRaw = "$secret://vault/http-logger/blank"
	blankConfig := Config{URI: "http://127.0.0.1/logs", AuthHeader: new(blankRaw)}
	blankCapability, blankScope, _, closeBlank := newHTTPLoggerScopedSecretHarness(
		t, 29, "http-blank", blankConfig, map[string]string{blankRaw: " \t "},
	)
	defer closeBlank()
	blankPlugin := newRawHTTPLoggerPlugin(t, blankConfig)
	err := base.MaterializeScopedPluginSecrets(
		context.Background(), blankScope, blankCapability, blankPlugin,
	)
	if err == nil || !strings.Contains(err.Error(), "credential unavailable") {
		t.Fatalf("blank materialization error = %v, want credential unavailable", err)
	}
	if blankPlugin.config.AuthHeader == nil || *blankPlugin.config.AuthHeader != blankRaw ||
		blankPlugin.secretsPrepared || blankPlugin.authHeaderSet {
		t.Fatal("blank resolved authorization installed partial state")
	}

	const failedRaw = "$secret://vault/http-logger/failure"
	failedConfig := Config{URI: "http://127.0.0.1/logs", AuthHeader: new(failedRaw)}
	capabilityValue, scope, broker, closeAttempt := newHTTPLoggerScopedSecretHarness(
		t, 30, "http-retry", failedConfig, map[string]string{failedRaw: "Bearer recovered"},
	)
	defer closeAttempt()
	broker.fail[failedRaw] = errors.New("resolver leaked " + failedRaw)
	p := newRawHTTPLoggerPlugin(t, failedConfig)
	err = base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || !strings.Contains(err.Error(), "credential unavailable") ||
		strings.Contains(err.Error(), failedRaw) {
		t.Fatalf("first materialization error = %v, want redacted unavailable", err)
	}
	if p.config.AuthHeader == nil || *p.config.AuthHeader != failedRaw ||
		p.client != nil || p.BatchProcessor != nil {
		t.Fatal("failed materialization installed partial public or runtime state")
	}
	broker.mu.Lock()
	delete(broker.fail, failedRaw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if p.config.AuthHeader == nil || *p.config.AuthHeader != httpAuthorizationDescriptor("Bearer recovered") {
		t.Fatalf("retry auth_header = %#v, want resolved descriptor", p.config.AuthHeader)
	}
}

func TestStopPreventsHandlerAndLogPhaseFromEnqueueing(t *testing.T) {
	p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	p.Stop()
	before := len(p.FireChan)

	handlerDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/stopped", nil))
		close(handlerDone)
	}()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("handler blocked after Stop")
	}
	if got := len(p.FireChan); got != before {
		t.Fatalf("handler FireChan length = %d, want unchanged %d after Stop", got, before)
	}

	err := p.RunLogPhase(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Method: http.MethodGet, URI: "/stopped"},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusNoContent},
	})
	if !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("RunLogPhase() error = %v, want queue unavailable after Stop", err)
	}
	if got := len(p.FireChan); got != before {
		t.Fatalf("log phase FireChan length = %d, want unchanged %d after Stop", got, before)
	}
	p.Stop()
}

func TestConcurrentHandlerAndLogPhaseStopWithoutQueueResurrection(t *testing.T) {
	for iteration := range 20 {
		p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})
		handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/race", nil))
		}()
		go func() {
			defer workers.Done()
			<-start
			_ = p.RunLogPhase(base.LogSnapshot{
				Request: apisixlog.RequestLogSnapshot{Method: http.MethodGet, URI: "/race"},
				Outcome: apisixctx.ResponseOutcome{Status: http.StatusNoContent},
			})
		}()
		close(start)
		p.Stop()
		workers.Wait()
		before := len(p.FireChan)
		if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
			t.Fatalf("iteration %d: post-Stop RunLogPhase() error = %v", iteration, err)
		}
		if got := len(p.FireChan); got != before {
			t.Fatalf("iteration %d: post-Stop FireChan length = %d, want %d", iteration, got, before)
		}
	}
}

func TestHTTPLoggerInstancesShareNeutralClientWithoutAuthorizationBleed(t *testing.T) {
	received := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	newScoped := func(revision uint64, resourceID, raw, resolved string) (*Plugin, func()) {
		config := Config{URI: server.URL, AuthHeader: new(raw), BatchMaxSize: 1}
		capabilityValue, scope, _, closeAttempt := newHTTPLoggerScopedSecretHarness(
			t, revision, resourceID, config, map[string]string{raw: resolved},
		)
		p := newRawHTTPLoggerPlugin(t, config)
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			t.Fatal(err)
		}
		if err := p.PostInit(); err != nil {
			t.Fatal(err)
		}
		return p, func() {
			p.Stop()
			closeAttempt()
		}
	}

	first, closeFirst := newScoped(51, "http-client-first", "$secret://http/first", "Bearer first")
	second, closeSecond := newScoped(52, "http-client-second", "$secret://http/second", "Bearer second")
	t.Cleanup(closeSecond)
	if first.client != second.client {
		t.Fatal("structurally identical instances did not share the neutral HTTP client")
	}
	if got := first.client.Header.Get("Authorization"); got != "" {
		t.Fatalf("shared client retained Authorization = %q", got)
	}
	if _, err := first.SendBatch(context.Background(), []map[string]any{{"generation": 51}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Bearer first" {
		t.Fatalf("first Authorization = %q", got)
	}
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": 52}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-received; got != "Bearer second" {
		t.Fatalf("second Authorization = %q", got)
	}

	closeFirst()
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": "52-again"}}, 1); err != nil {
		t.Fatalf("second instance after first Stop: %v", err)
	}
	if got := <-received; got != "Bearer second" {
		t.Fatalf("second Authorization after first Stop = %q", got)
	}
}

func TestSendBatchScrubsRetainedAuthorizationAndBodyState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	authorization := "Bearer retained-private"
	p := &Plugin{config: Config{URI: server.URL, AuthHeader: &authorization, ConcatMethod: "json"}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatal(err)
	}
	p.client = client
	t.Cleanup(p.Stop)
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"private": "request"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	if retainedRequest == nil || retainedResponse == nil || retainedRawResponse == nil {
		t.Fatal("Resty hook did not retain request, response, and raw response state")
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
			"retained raw body = %#v GetBody present = %t, want scrubbed",
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
			"retained raw response Body = %#v Request present = %t, want detached",
			retainedRawResponse.Body, retainedRawResponse.Request != nil,
		)
	}
}

func TestStopDrainsActiveSendAndDropsPrivateAuthorization(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	raw := "$secret://http/active"
	config := Config{URI: server.URL, AuthHeader: &raw, BatchMaxSize: 1}
	capabilityValue, scope, _, closeAttempt := newHTTPLoggerScopedSecretHarness(
		t, 61, "http-active", config, map[string]string{raw: "Bearer active"},
	)
	defer closeAttempt()
	p := newRawHTTPLoggerPlugin(t, config)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}

	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("active send did not reach backend")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active send drained")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseRequest)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after active send drained")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil ||
		p.authHeaderSet || p.legacyAuthHeader != nil || p.secretsPrepared {
		t.Fatalf("private/runtime state survived Stop: %#v", p)
	}
	if _, err := p.SendBatch(
		context.Background(),
		[]map[string]any{{"late": true}},
		1,
	); !errors.Is(
		err,
		secret.ErrCredentialUnavailable,
	) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	if err := p.MaterializeScopedSecrets(
		context.Background(),
		base.ScopedSecretAccess{},
	); !errors.Is(
		err,
		secret.ErrCredentialUnavailable,
	) {
		t.Fatalf("post-Stop materialization error = %v", err)
	}
	p.Stop()
}

func TestRunLogPhasePreservesDefaultFieldsAndRouteLabels(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{routeLabels: map[string]any{"team": "edge"}}
	p.logFormat = map[string]any{"labels": "$a6_route_labels", "remote": "$remote_addr"}
	p.SetSnapshotLogFormat(p.logFormat, nil)
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet, URI: "/orders", RemoteAddr: "192.0.2.3:1234",
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusUnauthorized},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		if fields["remote"] != "192.0.2.3" {
			t.Fatalf("remote = %#v", fields["remote"])
		}
		if labels, ok := fields["labels"].(map[string]any); !ok || labels["team"] != "edge" {
			t.Fatalf("labels = %#v", fields["labels"])
		}
	case <-time.After(time.Second):
		t.Fatal("detached HTTP entry was not delivered")
	}

	p.logFormat = nil
	fields := p.defaultSnapshotLogFields(snapshot)
	if fields["route_id"] != "no-matched" || fields["response"].(map[string]any)["status"] != http.StatusUnauthorized ||
		fields["request"].(map[string]any)["method"] != http.MethodGet || fields["server"] == nil ||
		fields["client_ip"] != "192.0.2.3" {
		t.Fatalf("default fields = %#v", fields)
	}
}

func TestRunLogPhaseDoesNotExposeBodyWhenExpressionDoesNotMatch(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{config: Config{
		IncludeReqBody: true, IncludeReqBodyExpr: []any{[]any{"status", "==", 500}},
		IncludeRespBody: true, IncludeRespBodyExpr: []any{[]any{"status", "==", 500}},
		MaxReqBodyBytes: 32, MaxRespBodyBytes: 32,
	}}
	p.logFormat = map[string]any{"request": "$request_body", "response": "$response_body"}
	p.SetSnapshotLogFormat(p.logFormat, nil)
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{
		Request:  apisixlog.RequestLogSnapshot{Body: []byte("private-request")},
		Response: apisixlog.ResponseLogSnapshot{Body: []byte("private-response")},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusOK},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		if fields["request"] != "" || fields["response"] != "" {
			t.Fatalf("body fields = %#v, want hidden bodies", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("detached HTTP entry was not delivered")
	}
}

func TestStopDefersHTTPClientReleaseUntilDeliveryCallbackReturns(t *testing.T) {
	started := make(chan struct{})
	releaseCallback := make(chan struct{})
	clientReleased := make(chan struct{})
	p := &Plugin{}
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
		BatchMaxSize:    1,
		ShutdownTimeout: 20 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(ctx context.Context, _ []map[string]any, _ int) (int, error) {
		close(started)
		<-ctx.Done()
		<-releaseCallback
		return 0, ctx.Err()
	})
	p.clientRelease = func() { close(clientReleased) }
	if !p.BatchProcessor.Push(map[string]any{"id": "blocked"}) {
		t.Fatal("push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Plugin.Stop() exceeded the processor shutdown bound")
	}
	select {
	case <-clientReleased:
		t.Fatal("HTTP client was released before delivery callback exit")
	default:
	}
	close(releaseCallback)
	select {
	case <-clientReleased:
	case <-time.After(time.Second):
		t.Fatal("HTTP client was not released after delivery callback exit")
	}
}

func TestSendBatchCancelsRestyRequestWithContext(t *testing.T) {
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

	p := newTestPlugin(t, Config{URI: server.URL, Timeout: 10})
	t.Cleanup(p.BatchProcessor.Stop)
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
		t.Fatal("timed out waiting for HTTP logger request")
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
	return newTestPluginWithMetadata(t, cfg, runtime.MetadataView{})
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata runtime.MetadataView) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata:       metadata,
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	nSource := []byte(`{"log_format":{"generation":"n","nested":{"level":"n"}},"max_pending_entries":31}`)
	nView := mustMetadataView(t, map[string][]byte{name: nSource})
	clear(nSource)
	n := newTestPluginWithMetadata(t, Config{URI: "http://127.0.0.1/n"}, nView)

	n1Source := []byte(`{"log_format":{"generation":"n1","nested":{"level":"n1"}},"max_pending_entries":32}`)
	n1View := mustMetadataView(t, map[string][]byte{name: n1Source})
	clear(n1Source)
	n1 := newTestPluginWithMetadata(t, Config{URI: "http://127.0.0.1/n1"}, n1View)

	if got := n.logFormat["generation"]; got != "n" || n.config.MaxPendingEntries != 31 {
		t.Fatalf("N metadata = format %#v pending %d, want n/31", got, n.config.MaxPendingEntries)
	}
	if got := n1.logFormat["generation"]; got != "n1" || n1.config.MaxPendingEntries != 32 {
		t.Fatalf("N+1 metadata = format %#v pending %d, want n1/32", got, n1.config.MaxPendingEntries)
	}

	route := map[string]any{"route": "$route_id"}
	routePlugin := newTestPluginWithMetadata(t, Config{
		URI: "http://127.0.0.1/route", LogFormat: route,
	}, n1View)
	if got := routePlugin.logFormat["route"]; got != "$route_id" || len(routePlugin.logFormat) != 1 {
		t.Fatalf("route format = %#v, want route precedence", routePlugin.logFormat)
	}
}

func TestPostInitRejectsInvalidMetadataBeforeSideEffects(t *testing.T) {
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs"}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string][]byte{
			name: []byte(`{"log_format":"sensitive-invalid-metadata"}`),
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	t.Cleanup(p.Stop)
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "http-logger metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want redacted metadata decode failure", err)
	}
	if strings.Contains(err.Error(), "sensitive-invalid-metadata") {
		t.Fatalf("PostInit() leaked metadata: %v", err)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil || p.config.Timeout != 0 {
		t.Fatalf(
			"PostInit() published side effects after invalid metadata: client=%v release=%t batch=%v timeout=%d",
			p.client,
			p.clientRelease != nil,
			p.BatchProcessor,
			p.config.Timeout,
		)
	}
}

func mustMetadataView(t *testing.T, documents map[string][]byte) runtime.MetadataView {
	t.Helper()
	view, err := runtime.NewMetadataView(documents)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestMaterializeSecretsRejectsMissingDataEncryptionResolver(t *testing.T) {
	authHeader := "Bearer private"
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	if err := p.MaterializeSecrets(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("MaterializeSecrets() error = %v, want missing resolver error", err)
	}
}

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})

	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want official default 3 seconds", p.config.Timeout)
	}
	if p.config.ConcatMethod != "json" {
		t.Fatalf("concat_method = %q, want json", p.config.ConcatMethod)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.MaxRetryCount != 0 {
		t.Fatalf("max_retry_count = %d, want 0", p.config.MaxRetryCount)
	}
}

func TestConfigPreservesExplicitZeroRetryDelay(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"uri":"http://127.0.0.1/logs","retry_delay":0}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	p := newTestPlugin(t, cfg)
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay = %d, want explicit zero", p.config.RetryDelay)
	}
	if !p.config.retryDelaySet {
		t.Fatal("config lost explicit retry_delay presence")
	}
}

func TestPostInitNormalizesOfficialInBodyExpression(t *testing.T) {
	p := newTestPlugin(t, Config{
		URI: "http://127.0.0.1/logs",
		IncludeRespBodyExpr: []any{
			[]any{"http_content_length", "<", float64(1024)},
			[]any{
				"http_content_type",
				"in",
				[]any{"application/xml", "application/json", "text/plain", "text/xml"},
			},
		},
	})

	second := p.config.IncludeRespBodyExpr[1].([]any)
	if second[1] != "~" {
		t.Fatalf("normalized operator = %#v, want regex match", second[1])
	}
	if second[2] != `^(application/xml|application/json|text/plain|text/xml)$` {
		t.Fatalf("normalized expression = %#v", second[2])
	}
}

func TestMaterializeSecretsRejectsInvalidEncryptedAuthHeader(t *testing.T) {
	authHeader := "not-a-ciphertext"
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want strict encrypted auth_header rejection")
	}
}

func TestMaterializeSecretsOwnsEncryptedAuthHeader(t *testing.T) {
	key := "qeddd145sfvddff3"
	authHeader := encryptHTTPLoggerTestValue(t, key, "Bearer secret")
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(true, []string{key}).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.AuthHeader == nil || *p.config.AuthHeader != httpAuthorizationDescriptor("Bearer secret") {
		t.Fatalf("auth_header = %v, want resolved descriptor", p.config.AuthHeader)
	}
	if p.legacyAuthHeader == nil {
		t.Fatal("legacy private auth_header was not retained by its owner")
	}
}

func TestSendPostsJSONLogWithAuthorizationHeader(t *testing.T) {
	authHeader := "Bearer secret"
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != authHeader {
			t.Fatalf("authorization = %q, want %q", r.Header.Get("Authorization"), authHeader)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:        server.URL + "/logs?source=apisix",
		AuthHeader: &authHeader,
		Timeout:    3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if body["path"] != "/orders" {
			t.Fatalf("body = %#v, want path /orders", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func encryptHTTPLoggerTestValue(t *testing.T, key string, value string) string {
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

func TestPostInitSetsTextContentTypeForNewLineConcat(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		ConcatMethod: "new_line",
		Timeout:      3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case got := <-received:
		if got != "text/plain" {
			t.Fatalf("content-type = %q, want text/plain", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerBatchesJSONLogs(t *testing.T) {
	received := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		if len(body) != 2 {
			t.Fatalf("batch length = %d, want 2", len(body))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerBatchesNewLineLogs(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		ConcatMethod:    "new_line",
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("body = %q, want two newline-delimited JSON entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("line %q is not JSON: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerDropsWhenMaxPendingEntriesExceeded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:               server.URL,
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   60,
		BufferDuration:    60,
	})
	t.Cleanup(func() {
		close(release)
		p.BatchProcessor.Stop()
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/three", nil))

	stats := p.BatchProcessor.Stats()
	if stats.Dropped != 2 {
		t.Fatalf("dropped = %d, want 2", stats.Dropped)
	}
}

func TestBatchProcessorLifecycleStateMatchesStaleAndBufferedCases(t *testing.T) {
	t.Run("completed delivery worker is removed while processor remains usable", func(t *testing.T) {
		received := make(chan struct{}, 2)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			received <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(server.Close)

		p := newTestPlugin(t, Config{URI: server.URL, BatchMaxSize: 1})
		t.Cleanup(p.BatchProcessor.Stop)
		handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for first delivery")
		}
		deadline := time.Now().Add(time.Second)
		for {
			stats := p.BatchProcessor.Stats()
			if stats.Pending == 0 && stats.Processing == 0 && stats.Buffered == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("completed worker state = %+v, want no pending, processing, or buffered entries", stats)
			}
			time.Sleep(10 * time.Millisecond)
		}

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("processor was not usable after completed worker cleanup")
		}
	})

	t.Run("buffered processor remains in use past stale window", func(t *testing.T) {
		received := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			received <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(server.Close)

		p := newTestPlugin(t, Config{
			URI:             server.URL,
			BatchMaxSize:    2,
			InactiveTimeout: 5,
		})
		t.Cleanup(p.BatchProcessor.Stop)
		handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/first", nil))
		time.Sleep(1500 * time.Millisecond)
		stats := p.BatchProcessor.Stats()
		if stats.Pending != 1 || stats.Buffered != 1 || stats.Processing != 0 {
			t.Fatalf("buffered state = %+v, want one pending buffered entry and no delivery worker", stats)
		}

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/second", nil))
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for preserved two-entry batch")
		}
	})
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:              server.URL,
		BatchMaxSize:     1,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Body.String() != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream body preserved", rr.Body.String())
	}
	select {
	case body := <-upstreamBody:
		if body != `{"order":1}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
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
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
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
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-upstreamBody:
		if body != `{"order":3}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}
	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want default request object", body["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("request = %#v, want no logged request body", request)
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want default response object", body["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("response = %#v, want no logged response body", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerResolvesNestedLogFormatAndTruncatesAtDepthFive(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"nested": map[string]any{"method": "$request_method"},
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{
						"d": map[string]any{
							"e": map[string]any{"f": "too-deep"},
						},
					},
				},
			},
		},
	})
	t.Cleanup(p.BatchProcessor.Stop)

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "http://example.com/nested", nil))

	select {
	case body := <-received:
		nested, ok := body["nested"].(map[string]any)
		if !ok || nested["method"] != http.MethodPost {
			t.Fatalf("nested = %#v, want resolved request method", body["nested"])
		}
		a := body["a"].(map[string]any)
		b := a["b"].(map[string]any)
		c := b["c"].(map[string]any)
		d := c["d"].(map[string]any)
		e := d["e"].(map[string]any)
		if len(e) != 0 {
			t.Fatalf("depth-five object = %#v, want nested f truncated", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for nested http log")
	}
}

func TestHandlerResolvesFinalStatusWithoutCapturingResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"response": map[string]any{"status": "$status"},
		},
	})
	t.Cleanup(p.BatchProcessor.Stop)

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, capturingBody := w.(*base.ResponseRecorder); capturingBody {
			t.Error("handler received a response-body recorder without a body logging requirement")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/status", nil))

	select {
	case body := <-received:
		response, ok := body["response"].(map[string]any)
		if !ok || response["status"] != float64(http.StatusCreated) {
			t.Fatalf("response = %#v, want final status %d", body["response"], http.StatusCreated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for status log")
	}
}

func TestHandlerDecodesCompressedResponseBodies(t *testing.T) {
	for _, test := range []struct {
		name     string
		encoding string
		encode   func(*testing.T, string) string
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			encode: func(t *testing.T, value string) string {
				t.Helper()
				var buf bytes.Buffer
				writer := gzip.NewWriter(&buf)
				if _, err := writer.Write([]byte(value)); err != nil {
					t.Fatalf("gzip write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				return buf.String()
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			encode: func(t *testing.T, value string) string {
				t.Helper()
				var buf bytes.Buffer
				writer := brotli.NewWriter(&buf)
				if _, err := writer.Write([]byte(value)); err != nil {
					t.Fatalf("brotli write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("brotli close: %v", err)
				}
				return buf.String()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			received := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- body
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				URI:             server.URL,
				BatchMaxSize:    1,
				IncludeRespBody: true,
			})
			t.Cleanup(p.BatchProcessor.Stop)
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", test.encoding)
				_, _ = io.WriteString(w, test.encode(t, "hello world"))
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/compressed", nil))

			select {
			case body := <-received:
				response := body["response"].(map[string]any)
				if response["body"] != "hello world" {
					t.Fatalf("response body = %#v, want decoded body", response["body"])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for compressed response log")
			}
		})
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"batch_max_size":      10,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     2,
		"inactive_timeout":    1,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official batch fields: %v", err)
	}
}
