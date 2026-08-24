package google_cloud_logging

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

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

type googleScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type googleScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []googleScopedSecretCall
}

func (*googleScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*googleScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *googleScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, googleScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Google test value")
	}
	return value, nil
}

func (*googleScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *googleScopedSecretBroker) scopedCalls() []googleScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]googleScopedSecretCall(nil), broker.calls...)
}

func newGoogleScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	rawConfig map[string]any,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *googleScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	resourceJSON, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: rawConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: resourceJSON,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "google-test",
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
	broker := &googleScopedSecretBroker{
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
			t.Errorf("close Google scoped secret registration: %v", err)
		}
	}
}

func googlePrivateKeyDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%x", digest)
}

func googleInlineRawConfig(privateKey string) map[string]any {
	return map[string]any{
		"auth_config": map[string]any{
			"client_email": "svc@example.iam.gserviceaccount.com",
			"private_key":  privateKey,
			"project_id":   "project-a",
			"token_uri":    "https://oauth2.example.test/token",
		},
	}
}

func newRawGooglePlugin(t *testing.T, rawConfig map[string]any) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMaterializeScopedSecretsOwnsGooglePrivateKey(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	rotated := encryptGooglePrivateKeyTestValue(t, "old-keyring-item", pemKey)
	tests := []struct {
		name string
		raw  string
	}{
		{name: "rotated ciphertext", raw: rotated},
		{name: "environment reference", raw: "$ENV://GOOGLE_PRIVATE_KEY"},
		{name: "managed reference", raw: "$secret://vault/google/private-key"},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawConfig := googleInlineRawConfig(tt.raw)
			capabilityValue, scope, broker, closeAttempt := newGoogleScopedSecretHarness(
				t, uint64(index+1), "google-inline", rawConfig,
				map[string]string{tt.raw: pemKey},
			)
			defer closeAttempt()
			p := newRawGooglePlugin(t, rawConfig)
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			calls := broker.scopedCalls()
			if len(calls) != 1 {
				t.Fatalf("scoped calls = %#v, want one private key", calls)
			}
			wantScope := scope
			wantScope.Field = "auth_config.private_key"
			if calls[0].Scope != wantScope || calls[0].Raw != tt.raw {
				t.Fatalf("scoped call = %#v, want %#v raw %q", calls[0], wantScope, tt.raw)
			}
			if got, want := p.config.AuthConfig.PrivateKey, googlePrivateKeyDescriptor(pemKey); got != want {
				t.Fatalf("public private_key = %q, want descriptor %q", got, want)
			}
			if p.resolvedAuth == nil || p.resolvedAuth.PrivateKey != pemKey {
				t.Fatal("materialization did not install private resolved auth")
			}
			if p.client != nil || p.BatchProcessor != nil {
				t.Fatal("materialization caused PostInit side effects")
			}
		})
	}

	authFileConfig := map[string]any{"auth_file": "/private/google-auth.json"}
	capabilityValue, scope, broker, closeAttempt := newGoogleScopedSecretHarness(
		t, 10, "google-auth-file", authFileConfig, nil,
	)
	defer closeAttempt()
	authFilePlugin := newRawGooglePlugin(t, authFileConfig)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, authFilePlugin,
	); err != nil {
		t.Fatalf("auth_file materialization error = %v", err)
	}
	if calls := broker.scopedCalls(); len(calls) != 0 {
		t.Fatalf("auth_file scoped calls = %#v, want none", calls)
	}
	if authFilePlugin.config.AuthFile != "/private/google-auth.json" ||
		authFilePlugin.config.AuthConfig != nil || authFilePlugin.resolvedAuth != nil {
		t.Fatal("auth_file materialization changed file-backed configuration")
	}

	for _, failure := range []struct {
		name        string
		raw         string
		resolved    string
		resolverErr error
	}{
		{
			name: "invalid resolved private key", raw: "$ENV://GOOGLE_INVALID_KEY",
			resolved: "not-a-private-key",
		},
		{
			name: "resolver failure", raw: "$secret://vault/google/failure",
			resolverErr: errors.New("resolver leaked $secret://vault/google/failure"),
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			rawConfig := googleInlineRawConfig(failure.raw)
			capabilityValue, scope, broker, closeAttempt := newGoogleScopedSecretHarness(
				t, 20, "google-retry", rawConfig,
				map[string]string{failure.raw: failure.resolved},
			)
			defer closeAttempt()
			if failure.resolverErr != nil {
				broker.fail[failure.raw] = failure.resolverErr
			}
			p := newRawGooglePlugin(t, rawConfig)
			err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
			if err == nil || !strings.Contains(err.Error(), "credential unavailable") {
				t.Fatalf("first materialization error = %v, want redacted credential unavailable", err)
			}
			if strings.Contains(err.Error(), failure.raw) ||
				(failure.resolved != "" && strings.Contains(err.Error(), failure.resolved)) {
				t.Fatalf("materialization error leaked private input: %v", err)
			}
			if p.config.AuthConfig.PrivateKey != failure.raw || p.resolvedAuth != nil ||
				p.client != nil || p.BatchProcessor != nil {
				t.Fatal("failed materialization installed partial auth or runtime state")
			}
			broker.mu.Lock()
			delete(broker.fail, failure.raw)
			broker.values[failure.raw] = pemKey
			broker.mu.Unlock()
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("same-instance retry error = %v", err)
			}
			if got, want := p.config.AuthConfig.PrivateKey, googlePrivateKeyDescriptor(pemKey); got != want {
				t.Fatalf("retry private_key = %q, want %q", got, want)
			}
		})
	}
}

func TestRunLogPhaseBuildsDefaultGoogleEntryFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{}
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
			Method: http.MethodPost, URL: "https://gateway.example/orders?x=1",
			URI: "/orders?x=1", Host: "gateway.example", RemoteAddr: "192.0.2.8:443",
			Header: http.Header{"User-Agent": {"test-agent"}}, ContentLength: 17,
			APISIXVars: map[string]any{"$route_id": "route-1"},
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusCreated, Bytes: 23},
		Started: time.Unix(10, 0), Finished: time.Unix(11, 500000000),
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		if entry[defaultEntryMarker] != true || entry[defaultRequestMethodField] != http.MethodPost {
			t.Fatalf("default marker/method = %#v/%#v", entry[defaultEntryMarker], entry[defaultRequestMethodField])
		}
		if entry[defaultRequestURLField] != "https://gateway.example/orders?x=1" ||
			entry[defaultStatusField] != http.StatusCreated {
			t.Fatalf("default URL/status = %#v/%#v", entry[defaultRequestURLField], entry[defaultStatusField])
		}
	case <-time.After(time.Second):
		t.Fatal("detached Google entry was not delivered")
	}
}

func TestSendBatchCancelsGoogleEntriesPostWithContext(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
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

	sslVerify := false
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			EntriesURI:  server.URL,
		},
		SSLVerify: &sslVerify,
	})
	t.Cleanup(p.BatchProcessor.Stop)
	p.tokenMu.Lock()
	p.accessToken = "cached-token"
	p.tokenType = "Bearer"
	p.tokenExpires = time.Now().Add(time.Hour)
	p.tokenMu.Unlock()
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
		t.Fatal("timed out waiting for Google Cloud Logging request")
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

func TestStopDrainsActiveGoogleSendAndDropsPrivateState(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	for index, mode := range []string{"legacy", "scoped"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				server.Close()
			})

			rawPrivateKey := pemKey
			if mode == "scoped" {
				rawPrivateKey = "$secret://vault/google/stop"
			}
			rawConfig := googleInlineRawConfig(rawPrivateKey)
			rawConfig["auth_config"].(map[string]any)["entries_uri"] = server.URL
			p := newRawGooglePlugin(t, rawConfig)
			var closeAttempt func()
			if mode == "legacy" {
				p.SetDependencies(base.Dependencies{
					DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
				})
				if err := p.MaterializeSecrets(); err != nil {
					t.Fatal(err)
				}
				closeAttempt = func() {}
			} else {
				capabilityValue, scope, _, closeScopedAttempt := newGoogleScopedSecretHarness(
					t, uint64(40+index), "google-stop", rawConfig,
					map[string]string{rawPrivateKey: pemKey},
				)
				closeAttempt = closeScopedAttempt
				if err := base.MaterializeScopedPluginSecrets(
					context.Background(), scope, capabilityValue, p,
				); err != nil {
					closeAttempt()
					t.Fatal(err)
				}
			}
			defer closeAttempt()
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			p.tokenMu.Lock()
			p.accessToken = "private-cached-token"
			p.tokenType = "Bearer"
			p.tokenExpires = time.Now().Add(time.Hour)
			p.tokenMu.Unlock()

			activeResult := make(chan error, 1)
			go func() {
				_, err := p.SendBatch(
					context.Background(), []map[string]any{{"active": true}}, 1,
				)
				activeResult <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for active Google send")
			}

			stopDone := make(chan struct{})
			go func() {
				p.Stop()
				close(stopDone)
			}()
			select {
			case <-stopDone:
				releaseOnce.Do(func() { close(release) })
				t.Fatal("Stop returned before the active Google send drained")
			case <-time.After(100 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-activeResult:
				if err != nil {
					t.Fatalf("active SendBatch() error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("active Google send did not finish")
			}
			select {
			case <-stopDone:
			case <-time.After(time.Second):
				t.Fatal("Stop did not finish after active Google send drained")
			}

			p.tokenMu.Lock()
			retainedToken := p.accessToken
			p.tokenMu.Unlock()
			if !p.stopped.Load() || p.resolvedAuth != nil || retainedToken != "" ||
				p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
				t.Fatal("Stop retained Google private auth, token, client, or processor state")
			}
			if _, err := p.SendBatch(
				context.Background(), []map[string]any{{"late": true}}, 1,
			); !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("post-Stop SendBatch() error = %v, want credential unavailable", err)
			}
			if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("post-Stop materialization error = %v, want credential unavailable", err)
			}
			p.Stop()
		})
	}
}

func TestGoogleStopRejectsRetainedLogCallbacks(t *testing.T) {
	p := newTestPlugin(t, Config{})
	retainedHandler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	p.Stop()

	before := len(p.FireChan)
	handlerDone := make(chan struct{})
	go func() {
		retainedHandler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "http://gateway.test/retained", nil),
		)
		close(handlerDone)
	}()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("retained Handler blocked after Stop returned")
	}

	runDone := make(chan error, 1)
	go func() {
		runDone <- p.RunLogPhase(base.LogSnapshot{})
	}()
	select {
	case err := <-runDone:
		if !errors.Is(err, base.ErrLogQueueUnavailable) {
			t.Fatalf("post-Stop RunLogPhase() error = %v, want unavailable queue", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retained RunLogPhase blocked after Stop returned")
	}
	if got := len(p.FireChan); got != before {
		t.Fatalf("post-Stop retained callbacks changed FireChan length from %d to %d", before, got)
	}
}

func TestGoogleStopRejectsRetainedFormattedHandler(t *testing.T) {
	p := newTestPlugin(t, Config{})
	p.LogFormat = map[string]string{"method": "$request_method"}
	retainedHandler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	p.Stop()

	before := len(p.FireChan)
	retainedHandler.ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://gateway.test/formatted", nil),
	)
	if got := len(p.FireChan); got != before {
		t.Fatalf("post-Stop formatted Handler changed FireChan length from %d to %d", before, got)
	}
}

func TestGoogleConcurrentLogCallbacksAndStop(t *testing.T) {
	p := newTestPlugin(t, Config{})
	releaseHandlers := make(chan struct{})
	retainedHandler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-releaseHandlers
	}))

	var handlers sync.WaitGroup
	for range 32 {
		handlers.Go(func() {
			retainedHandler.ServeHTTP(
				httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "http://gateway.test/concurrent", nil),
			)
		})
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	for !p.stopped.Load() {
		time.Sleep(time.Millisecond)
	}
	close(releaseHandlers)

	var phases sync.WaitGroup
	for range 32 {
		phases.Go(func() {
			for range 32 {
				_ = p.RunLogPhase(base.LogSnapshot{})
			}
		})
	}
	handlers.Wait()
	phases.Wait()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish with concurrent retained log callbacks")
	}
	if got := len(p.FireChan); got != 0 {
		t.Fatalf("concurrent post-Stop callbacks enqueued %d legacy FireChan entries", got)
	}
}

func TestGoogleGenerationsShareOnlyCredentialNeutralClient(t *testing.T) {
	firstPEM, _ := testPrivateKey(t)
	secondPEM, _ := testPrivateKey(t)
	authorizations := make(chan string, 3)
	entriesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entriesServer.Close)

	newGeneration := func(
		revision uint64, rawPrivateKey, resolvedPrivateKey string,
	) (*Plugin, func()) {
		rawConfig := googleInlineRawConfig(rawPrivateKey)
		authConfig := rawConfig["auth_config"].(map[string]any)
		authConfig["entries_uri"] = entriesServer.URL
		sslVerify := false
		rawConfig["ssl_verify"] = sslVerify
		capabilityValue, scope, _, closeAttempt := newGoogleScopedSecretHarness(
			t, revision, "same-google-route", rawConfig,
			map[string]string{rawPrivateKey: resolvedPrivateKey},
		)
		p := newRawGooglePlugin(t, rawConfig)
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
		return p, closeAttempt
	}

	first, closeFirst := newGeneration(60, "$ENV://GOOGLE_KEY_N", firstPEM)
	defer closeFirst()
	second, closeSecond := newGeneration(61, "$secret://vault/google/key-n1", secondPEM)
	defer closeSecond()
	t.Cleanup(first.Stop)
	t.Cleanup(second.Stop)
	if first.client != second.client {
		t.Fatal("two generations did not reuse the credential-neutral Resty client")
	}
	if first.resolvedAuth == nil || second.resolvedAuth == nil ||
		first.resolvedAuth.PrivateKey == second.resolvedAuth.PrivateKey {
		t.Fatal("two generations reused private resolved auth")
	}

	setToken := func(p *Plugin, token string) {
		p.tokenMu.Lock()
		p.accessToken = token
		p.tokenType = "Bearer"
		p.tokenExpires = time.Now().Add(time.Hour)
		p.tokenMu.Unlock()
	}
	setToken(first, "generation-n-token")
	setToken(second, "generation-n1-token")
	if _, err := first.SendBatch(
		context.Background(), []map[string]any{{"generation": "n"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendBatch(
		context.Background(), []map[string]any{{"generation": "n1"}}, 1,
	); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Bearer generation-n-token", "Bearer generation-n1-token"} {
		select {
		case got := <-authorizations:
			if got != want {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	first.Stop()
	if _, err := second.SendBatch(
		context.Background(), []map[string]any{{"generation": "n1-after-stop"}}, 1,
	); err != nil {
		t.Fatalf("N+1 send after N Stop error = %v", err)
	}
	select {
	case got := <-authorizations:
		if got != "Bearer generation-n1-token" {
			t.Fatalf("N+1 Authorization after N Stop = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for N+1 send after N Stop")
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
	nSource := []byte(`{"log_format":{"generation":"n"},"max_pending_entries":21}`)
	nView := mustMetadataView(t, map[string][]byte{name: nSource})
	clear(nSource)
	n := newTestPluginWithMetadata(t, Config{}, nView)

	n1Source := []byte(`{"log_format":{"generation":"n1"},"max_pending_entries":22}`)
	n1View := mustMetadataView(t, map[string][]byte{name: n1Source})
	clear(n1Source)
	n1 := newTestPluginWithMetadata(t, Config{}, n1View)

	if got := n.LogFormat["generation"]; got != "n" || n.config.MaxPendingEntries != 21 {
		t.Fatalf("N metadata = format %q pending %d, want n/21", got, n.config.MaxPendingEntries)
	}
	if got := n1.LogFormat["generation"]; got != "n1" || n1.config.MaxPendingEntries != 22 {
		t.Fatalf("N+1 metadata = format %q pending %d, want n1/22", got, n1.config.MaxPendingEntries)
	}

	route := map[string]string{"route": "$route_id"}
	routePlugin := newTestPluginWithMetadata(t, Config{LogFormat: route}, n1View)
	if got := routePlugin.LogFormat["route"]; got != "$route_id" || len(routePlugin.LogFormat) != 1 {
		t.Fatalf("route format = %#v, want route precedence", routePlugin.LogFormat)
	}
}

func TestPostInitRejectsInvalidMetadataBeforeSideEffects(t *testing.T) {
	p := &Plugin{}
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
	if err == nil || !strings.Contains(err.Error(), "google-cloud-logging metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want redacted metadata decode failure", err)
	}
	if strings.Contains(err.Error(), "sensitive-invalid-metadata") {
		t.Fatalf("PostInit() leaked metadata: %v", err)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil || p.config.Resource.Type != "" {
		t.Fatalf(
			"PostInit() published side effects after invalid metadata: client=%v release=%t batch=%v resource=%q",
			p.client,
			p.clientRelease != nil,
			p.BatchProcessor,
			p.config.Resource.Type,
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
	p := &Plugin{config: Config{AuthConfig: &AuthConfig{PrivateKey: "private"}}}
	if err := p.MaterializeSecrets(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("MaterializeSecrets() error = %v, want missing resolver error", err)
	}
}

func TestPostInitSetsGoogleDefaults(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
	})

	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.Resource.Type != "global" {
		t.Fatalf("resource.type = %q, want global", p.config.Resource.Type)
	}
	if p.config.LogID != "apisix.apache.org%2Flogs" {
		t.Fatalf("log_id = %q, want apisix.apache.org%%2Flogs", p.config.LogID)
	}
	if p.config.AuthConfig.EntriesURI != "https://logging.googleapis.com/v2/entries:write" {
		t.Fatalf("entries_uri = %q, want default Google entries endpoint", p.config.AuthConfig.EntriesURI)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestMetadataSchemaAcceptsObjectLogFormatAndRejectsString(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}
	valid := map[string]any{
		"log_format":          map[string]any{"host": "$host"},
		"max_pending_entries": 1,
	}
	if err := util.Validate(valid, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	if err := util.Validate(map[string]any{"log_format": "wrong-type"}, p.GetMetadataSchema()); err == nil {
		t.Fatal("string log_format was accepted")
	}
}

func TestPostInitLoadsSSLCAFileForVerifiedClient(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caFile := t.TempDir() + "/ca.pem"
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	t.Setenv("SSL_CERT_FILE", caFile)

	privateKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{AuthConfig: &AuthConfig{
		ClientEmail: "trusted-ca@example.org",
		PrivateKey:  privateKey,
		ProjectID:   "trusted-ca",
		TokenURI:    server.URL + "/token",
		EntriesURI:  server.URL + "/entries",
	}})
	t.Cleanup(p.Stop)

	response, err := p.client.R().Get(server.URL)
	if err != nil {
		t.Fatalf("verified request using SSL_CERT_FILE: %v", err)
	}
	if response.StatusCode() != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode(), http.StatusNoContent)
	}
}

func TestMaterializeSecretsRejectsInvalidEncryptedPrivateKey(t *testing.T) {
	p := &Plugin{config: Config{AuthConfig: &AuthConfig{PrivateKey: "not-a-ciphertext"}}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want strict encrypted private_key rejection")
	}
}

func TestMaterializeSecretsResolvesRotatedEncryptedPrivateKey(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	pemKey, _ := testPrivateKey(t)
	p := &Plugin{config: Config{AuthConfig: &AuthConfig{
		ClientEmail: "svc@example.iam.gserviceaccount.com",
		PrivateKey:  encryptGooglePrivateKeyTestValue(t, oldKey, pemKey),
		ProjectID:   "project-a",
	}}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
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
	if got, want := p.config.AuthConfig.PrivateKey, googlePrivateKeyDescriptor(pemKey); got != want {
		t.Fatalf("private_key = %q, want descriptor %q", got, want)
	}
	if p.resolvedAuth == nil || p.resolvedAuth.PrivateKey != pemKey {
		t.Fatal("legacy materialization did not retain private resolved auth")
	}
}

func TestServiceAccountAssertionUsesConfiguredClaims(t *testing.T) {
	pemKey, key := testPrivateKey(t)
	assertions := make(chan url.Values, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		assertions <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			Scopes:      []string{"scope-a", "scope-b"},
		},
	})

	auth, err := p.authConfig()
	if err != nil {
		t.Fatalf("authConfig() error = %v", err)
	}
	if _, _, err := p.accessTokenFor(context.Background(), auth); err != nil {
		t.Fatalf("accessTokenFor() error = %v", err)
	}

	var form url.Values
	select {
	case form = <-assertions:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for token request")
	}
	if form.Get("grant_type") != jwtBearerGrantType {
		t.Fatalf("grant_type = %q, want jwt bearer grant", form.Get("grant_type"))
	}
	assertion := form.Get("assertion")
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts, want 3", len(parts))
	}

	var header map[string]any
	mustDecodeJWTPart(t, parts[0], &header)
	if header["alg"] != "RS256" {
		t.Fatalf("alg = %v, want RS256", header["alg"])
	}

	var claims map[string]any
	mustDecodeJWTPart(t, parts[1], &claims)
	if claims["iss"] != "svc@example.iam.gserviceaccount.com" {
		t.Fatalf("iss = %v, want service account email", claims["iss"])
	}
	if claims["sub"] != "svc@example.iam.gserviceaccount.com" {
		t.Fatalf("sub = %v, want service account email", claims["sub"])
	}
	if claims["aud"] != tokenServer.URL {
		t.Fatalf("aud = %v, want token uri", claims["aud"])
	}
	if claims["scope"] != "scope-a scope-b" {
		t.Fatalf("scope = %v, want joined scopes", claims["scope"])
	}

	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], signature); err != nil {
		t.Fatalf("verify jwt signature: %v", err)
	}
}

func TestBuildEntryUsesCloudLoggingShape(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
		Resource: MonitoredResource{
			Type:   "global",
			Labels: map[string]string{"project_id": "project-a"},
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	entry := p.buildEntry(map[string]any{"path": "/orders"})
	if entry.LogName != "projects/project-a/logs/apisix.apache.org%2Flogs" {
		t.Fatalf("logName = %q, want project log name", entry.LogName)
	}
	if entry.Labels["source"] != "apache-apisix-google-cloud-logging" {
		t.Fatalf("source label = %q, want apache source label", entry.Labels["source"])
	}
	if entry.Resource.Type != "global" {
		t.Fatalf("resource.type = %q, want global", entry.Resource.Type)
	}
	if entry.JSONPayload["path"] != "/orders" {
		t.Fatalf("jsonPayload path = %v, want /orders", entry.JSONPayload["path"])
	}
	if entry.Timestamp == "" {
		t.Fatal("timestamp is empty")
	}
}

func TestHandlerBuildsDefaultHTTPRequestEntry(t *testing.T) {
	p := &Plugin{config: Config{
		AuthConfig: &AuthConfig{
			ProjectID: "project-a",
		},
		Resource: MonitoredResource{Type: "global"},
		LogID:    defaultLogID,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders?debug=true", strings.NewReader("payload"))
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("User-Agent", "apisix-go-test")
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":   "route-1",
		"$service_id": "service-1",
	})
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	var fields map[string]any
	select {
	case fields = <-p.FireChan:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for google log fields")
	}

	entry := p.buildEntry(fields)
	if entry.HTTPRequest == nil {
		t.Fatal("httpRequest is nil")
	}
	if entry.HTTPRequest.RequestMethod != http.MethodPost {
		t.Fatalf("requestMethod = %q, want POST", entry.HTTPRequest.RequestMethod)
	}
	if entry.HTTPRequest.RequestURL != "http://example.com/orders?debug=true" {
		t.Fatalf("requestUrl = %q, want full request URL", entry.HTTPRequest.RequestURL)
	}
	if entry.HTTPRequest.RequestSize != 7 {
		t.Fatalf("requestSize = %d, want 7", entry.HTTPRequest.RequestSize)
	}
	if entry.HTTPRequest.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", entry.HTTPRequest.Status)
	}
	if entry.HTTPRequest.ResponseSize != 7 {
		t.Fatalf("responseSize = %d, want 7", entry.HTTPRequest.ResponseSize)
	}
	if entry.HTTPRequest.UserAgent != "apisix-go-test" {
		t.Fatalf("userAgent = %q, want apisix-go-test", entry.HTTPRequest.UserAgent)
	}
	if entry.HTTPRequest.RemoteIP != "203.0.113.10" {
		t.Fatalf("remoteIp = %q, want 203.0.113.10", entry.HTTPRequest.RemoteIP)
	}
	if entry.HTTPRequest.Latency == "" {
		t.Fatal("latency is empty")
	}
	if entry.JSONPayload["route_id"] != "route-1" {
		t.Fatalf("route_id = %v, want route-1", entry.JSONPayload["route_id"])
	}
	if entry.JSONPayload["service_id"] != "service-1" {
		t.Fatalf("service_id = %v, want service-1", entry.JSONPayload["service_id"])
	}
}

func TestBuildEntryKeepsCustomLogFormatInJSONPayload(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    "http://127.0.0.1/token",
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	entry := p.buildEntry(map[string]any{"path": "/orders"})
	if entry.HTTPRequest != nil {
		t.Fatalf("httpRequest = %#v, want nil for custom log_format", entry.HTTPRequest)
	}
	if entry.JSONPayload["path"] != "/orders" {
		t.Fatalf("jsonPayload path = %v, want /orders", entry.JSONPayload["path"])
	}
}

func TestSendExchangesTokenAndWritesEntries(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryRequests := make(chan *http.Request, 1)
	entryBodies := make(chan map[string]any, 1)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode entries body: %v", err)
		}
		entryRequests <- r
		entryBodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		SSLVerify: &sslVerify,
		LogFormat: map[string]string{"path": "$uri"},
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case form := <-tokenRequests:
		if form.Get("grant_type") != jwtBearerGrantType {
			t.Fatalf("grant_type = %q, want jwt bearer grant", form.Get("grant_type"))
		}
		if form.Get("assertion") == "" {
			t.Fatal("assertion is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for token request")
	}

	select {
	case req := <-entryRequests:
		if got := req.Header.Get("Authorization"); got != "Bearer token-a" {
			t.Fatalf("Authorization = %q, want Bearer token-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entries request")
	}

	select {
	case body := <-entryBodies:
		if body["partialSuccess"] != false {
			t.Fatalf("partialSuccess = %v, want false", body["partialSuccess"])
		}
		entries, ok := body["entries"].([]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("entries = %#v, want one entry", body["entries"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for entries body")
	}
}

func TestSendBatchWritesGoogleEntries(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 1)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryBodies := make(chan map[string]any, 1)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode entries body: %v", err)
		}
		entryBodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-entryBodies:
		entries, ok := body["entries"].([]any)
		if !ok {
			t.Fatalf("entries = %#v, want array", body["entries"])
		}
		if len(entries) != 2 {
			t.Fatalf("entries length = %d, want 2", len(entries))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Google Cloud Logging entries request")
	}

	select {
	case <-tokenRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Google OAuth token request")
	}
}

func TestSendBatchReusesCachedAccessToken(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 2)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryRequests := make(chan string, 2)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entryRequests <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1); err != nil {
		t.Fatalf("first SendBatch() error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/b"}}, 1); err != nil {
		t.Fatalf("second SendBatch() error = %v", err)
	}

	for range 2 {
		select {
		case auth := <-entryRequests:
			if auth != "Bearer token-a" {
				t.Fatalf("Authorization = %q, want cached Bearer token-a", auth)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for entries request")
		}
	}

	select {
	case <-tokenRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first token request")
	}
	select {
	case extra := <-tokenRequests:
		t.Fatalf("unexpected second token request: %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSendBatchRefreshesExpiredAccessToken(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	tokenRequests := make(chan url.Values, 2)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		tokenRequests <- r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":1}`))
	}))
	t.Cleanup(tokenServer.Close)

	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	p := newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1); err != nil {
		t.Fatalf("first SendBatch() error = %v", err)
	}

	// Let the 1-second token expire before the next batch so the shared token
	// source is forced to refresh over the network.
	time.Sleep(1100 * time.Millisecond)

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/b"}}, 1); err != nil {
		t.Fatalf("second SendBatch() error = %v", err)
	}

	for i := range 2 {
		select {
		case <-tokenRequests:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for token request %d", i+1)
		}
	}
}

func testPrivateKey(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: mustMarshalPKCS8(t, key),
	})
	return string(pemKey), key
}

func encryptGooglePrivateKeyTestValue(t *testing.T, key string, value string) string {
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

func mustMarshalPKCS8(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()

	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8 key: %v", err)
	}
	return encoded
}

func mustDecodeJWTPart(t *testing.T, part string, v any) {
	t.Helper()

	data, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode jwt part: %v", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal jwt part: %v", err)
	}
}

func TestLoadAuthConfigFromFile(t *testing.T) {
	pemKey, _ := testPrivateKey(t)
	file := writeTempAuthFile(t, pemKey)
	p := newTestPlugin(t, Config{AuthFile: file})

	auth, err := p.authConfig()
	if err != nil {
		t.Fatalf("authConfig() error = %v", err)
	}
	if auth.ProjectID != "project-from-file" {
		t.Fatalf("project_id = %q, want project-from-file", auth.ProjectID)
	}
}

func writeTempAuthFile(t *testing.T, pemKey string) string {
	t.Helper()

	body := map[string]any{
		"client_email": "svc@example.iam.gserviceaccount.com",
		"private_key":  pemKey,
		"project_id":   "project-from-file",
		"token_uri":    "http://127.0.0.1/token",
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}

	file := t.TempDir() + "/auth.json"
	if err := writeFile(file, data); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return file
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func TestSendBatchUsesCachedAuthFileAfterRemoval(t *testing.T) {
	pemKey, _ := testPrivateKey(t)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	body := map[string]any{
		"client_email": "svc@example.iam.gserviceaccount.com",
		"private_key":  pemKey,
		"project_id":   "project-from-file",
		"token_uri":    tokenServer.URL,
		"entries_uri":  entryServer.URL,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	file := t.TempDir() + "/auth.json"
	if err := writeFile(file, data); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	p := newTestPlugin(t, Config{AuthFile: file})
	if err := os.Remove(file); err != nil {
		t.Fatalf("remove auth file: %v", err)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1); err != nil {
		t.Fatalf("SendBatch() after auth file removal error = %v, want cached auth config", err)
	}
}

func TestSendBatchTimesOutTokenEndpoint(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(tokenServer.Close)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(entryServer.Close)

	pemKey, _ := testPrivateKey(t)
	p := &Plugin{
		config: Config{AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		}},
		requestTimeout: 300 * time.Millisecond,
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
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

	start := time.Now()
	_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SendBatch() error = nil, want token endpoint timeout")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("SendBatch took %v, want bounded by the request timeout", elapsed)
	}
}

func TestSendBatchTimesOutWriteEndpoint(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(tokenServer.Close)
	entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(entryServer.Close)

	pemKey, _ := testPrivateKey(t)
	p := &Plugin{
		config: Config{AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  pemKey,
			ProjectID:   "project-a",
			TokenURI:    tokenServer.URL,
			EntriesURI:  entryServer.URL,
		}},
		requestTimeout: 300 * time.Millisecond,
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
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

	start := time.Now()
	_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SendBatch() error = nil, want write endpoint timeout")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("SendBatch took %v, want bounded by the request timeout", elapsed)
	}
}
