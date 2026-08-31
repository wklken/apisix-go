package oas_validator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type oasScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type oasScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []oasScopedSecretCall
	hook   func(oasScopedSecretCall)
}

func (broker *oasScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	call := oasScopedSecretCall{Scope: scope, Raw: raw}
	broker.calls = append(broker.calls, call)
	failure := broker.fail[raw]
	value, found := broker.values[raw]
	hook := broker.hook
	broker.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if failure != nil {
		return "", failure
	}
	if found {
		return value, nil
	}
	return raw, nil
}

func (broker *oasScopedSecretBroker) setHook(hook func(oasScopedSecretCall)) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.hook = hook
}

func (broker *oasScopedSecretBroker) callsSnapshot() []oasScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return slices.Clone(broker.calls)
}

func newOASScopedSecretHarness(
	t *testing.T, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *oasScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: "oas-scoped"}
	snapshot, err := generation.NewSnapshot(70, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: 70,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
		}},
	}
	set := generation.PublicationSet{
		DesiredRevision: 70,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &oasScopedSecretBroker{
		values: maps.Clone(values), fail: make(map[string]error),
	}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: 70, Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func materializeScopedOASSecrets(
	t *testing.T, p *Plugin, secrets secret.GenerationSecrets, scope secret.Scope,
) {
	t.Helper()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func oasSecretDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func assertOASScopedCalls(
	t *testing.T, scope secret.Scope, calls []oasScopedSecretCall, fields, raws []string,
) {
	t.Helper()
	if len(calls) != len(fields) || len(fields) != len(raws) {
		t.Fatalf("broker calls = %#v, want fields=%#v raws=%#v", calls, fields, raws)
	}
	for index := range calls {
		wantScope := scope
		wantScope.Field = fields[index]
		if calls[index].Scope != wantScope || calls[index].Raw != raws[index] {
			t.Fatalf("call[%d] = %#v, want scope=%#v raw=%q", index, calls[index], wantScope, raws[index])
		}
	}
}

func TestScopedSecretsMaterializeOASInlineSpec(t *testing.T) {
	const raw = "$ENV://OAS_SCOPED_INLINE_SPEC"
	spec := testSpec()
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: spec},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Spec: raw}}
	materializeScopedOASSecrets(t, p, secrets, scope)
	assertOASScopedCalls(t, scope, broker.callsSnapshot(), []string{"spec"}, []string{raw})
	if p.config.Spec != oasSecretDescriptor(spec) || strings.Contains(p.config.Spec, raw) {
		t.Fatalf("public inline spec = %q", p.config.Spec)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if _, err := p.validator(); err != nil {
		t.Fatalf("validator() error = %v", err)
	}
}

func TestScopedSecretsMaterializeOASHeaderContainerDeterministically(t *testing.T) {
	const (
		rawAuth = "$ENV://OAS_SCOPED_AUTH"
		rawZ    = "$ENV://OAS_SCOPED_Z"
	)
	var mu sync.Mutex
	paths := make([]string, 0, 2)
	specServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer resolved" || r.Header.Get("X-Z") != "z-resolved" {
			http.Error(w, "missing resolved headers", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/openapi.json":
			_, _ = w.Write([]byte(strings.Replace(
				testSpecWithComponentsRef(), "#/components/schemas/Pet",
				"schemas/pet.json#/components/schemas/Pet", 1,
			)))
		case "/schemas/pet.json":
			_, _ = w.Write(
				[]byte(
					`{"components":{"schemas":{"Pet":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}}}`,
				),
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer specServer.Close()
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(t, map[string]string{
		rawAuth: "Bearer resolved", rawZ: "z-resolved",
	})
	defer closeAttempt()
	p := &Plugin{config: Config{
		SpecURL:               specServer.URL + "/openapi.json",
		VerboseErrors:         true,
		SpecURLRequestHeaders: map[string]string{"X-Z": rawZ, "Authorization": rawAuth},
	}}
	materializeScopedOASSecrets(t, p, secrets, scope)
	assertOASScopedCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"spec_url_request_headers", "spec_url_request_headers"},
		[]string{rawAuth, rawZ},
	)
	if p.config.SpecURLRequestHeaders["Authorization"] != oasSecretDescriptor("Bearer resolved") ||
		p.config.SpecURLRequestHeaders["X-Z"] != oasSecretDescriptor("z-resolved") {
		t.Fatalf("public headers = %#v", p.config.SpecURLRequestHeaders)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/pets", strings.NewReader(`{"age":3}`))
	request.Header.Set("Content-Type", "application/json")
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached next handler")
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(paths, []string{"/openapi.json", "/schemas/pet.json"}) {
		t.Fatalf("authenticated document paths = %#v", paths)
	}
}

func TestScopedSecretsResolveManagedOASHeader(t *testing.T) {
	const managed = "$secret://vault/oas/authorization"
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer server.Close()
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{managed: "Bearer managed"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		SpecURL:               server.URL,
		SpecURLRequestHeaders: map[string]string{"Authorization": managed},
	}}
	materializeScopedOASSecrets(t, p, secrets, scope)
	assertOASScopedCalls(
		t, scope, broker.callsSnapshot(), []string{"spec_url_request_headers"}, []string{managed},
	)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	if _, err := p.validator(); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer managed" {
		t.Fatalf("remote Authorization = %q", authorization)
	}
}

func TestScopedSecretsOASHeaderFailureIsAtomic(t *testing.T) {
	const (
		rawSpec = "$ENV://OAS_ATOMIC_SPEC"
		rawA    = "$ENV://OAS_ATOMIC_A"
		rawZ    = "$ENV://OAS_ATOMIC_Z"
	)
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(t, map[string]string{
		rawSpec: testSpec(), rawA: "a-value", rawZ: "z-value",
	})
	defer closeAttempt()
	broker.fail[rawZ] = errors.New("injected header failure")
	p := &Plugin{config: Config{
		Spec: rawSpec, SpecURLRequestHeaders: map[string]string{"Z": rawZ, "A": rawA},
	}}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("materialization error = %v, want ErrCredentialUnavailable", err)
	}
	assertOASScopedCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"spec", "spec_url_request_headers", "spec_url_request_headers"},
		[]string{rawSpec, rawA, rawZ},
	)
	if p.config.Spec != rawSpec || !maps.Equal(
		p.config.SpecURLRequestHeaders, map[string]string{"Z": rawZ, "A": rawA},
	) {
		t.Fatalf("failed materialization changed config: %#v", p.config)
	}
	broker.mu.Lock()
	delete(broker.fail, rawZ)
	broker.calls = nil
	broker.mu.Unlock()
	materializeScopedOASSecrets(t, p, secrets, scope)
	assertOASScopedCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"spec", "spec_url_request_headers", "spec_url_request_headers"},
		[]string{rawSpec, rawA, rawZ},
	)
	p.Stop()
}

func TestPostInitDoesNotSelfMaterializeOASSecrets(t *testing.T) {
	const (
		specEnv   = "OAS_DIRECT_POSTINIT_SPEC"
		headerEnv = "OAS_DIRECT_POSTINIT_HEADER"
	)
	t.Setenv(specEnv, testSpec())
	t.Setenv(headerEnv, "Bearer must-not-resolve")
	rawSpec := "$ENV://" + specEnv
	rawHeader := "$ENV://" + headerEnv
	p := &Plugin{config: Config{
		Spec: rawSpec, SpecURLRequestHeaders: map[string]string{"Authorization": rawHeader},
	}}
	err := p.PostInit()
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want ErrCredentialUnavailable", err)
	}
	if strings.Contains(err.Error(), rawSpec) || strings.Contains(err.Error(), rawHeader) ||
		strings.Contains(err.Error(), "must-not-resolve") {
		t.Fatalf("PostInit() leaked secret material: %v", err)
	}
	if p.config.Spec != rawSpec || p.config.SpecURLRequestHeaders["Authorization"] != rawHeader {
		t.Fatalf("PostInit() self-materialized config: %#v", p.config)
	}
}

func TestOASStopDropsScopedSecrets(t *testing.T) {
	const raw = "$ENV://OAS_STOP_INLINE_SPEC"
	secrets, scope, _, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: testSpec()},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{Spec: raw}}
	materializeScopedOASSecrets(t, p, secrets, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.validator(); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	p.Stop()
	if p.scopedInline != (secret.Value{}) || p.scopedHeaders != nil ||
		p.scopedSet || len(p.headerNames) != 0 {
		t.Fatalf(
			"Stop() retained scoped secret state: scoped=%v headers=%d",
			p.scopedSet, len(p.headerNames),
		)
	}
	if _, err := p.validator(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop validator() error = %v", err)
	}
}

func TestOASStopWaitsForScopedHeaderFetch(t *testing.T) {
	const raw = "$ENV://OAS_BLOCKED_FETCH_HEADER"
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer blocked" {
			http.Error(w, "missing header", http.StatusUnauthorized)
			return
		}
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer server.Close()
	p := &Plugin{config: Config{
		SpecURL:               server.URL,
		SpecURLRequestHeaders: map[string]string{"Authorization": raw},
	}}
	secrets, scope, _, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: "Bearer blocked"},
	)
	defer closeAttempt()
	materializeScopedOASSecrets(t, p, secrets, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	validatorDone := make(chan error, 1)
	go func() {
		_, err := p.validator()
		validatorDone <- err
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(release)
		<-validatorDone
		t.Fatal("Stop() returned before header-bearing fetch completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-validatorDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("validator() error = %v, want %v", err, secret.ErrCredentialUnavailable)
	}
	<-stopDone
	if p.scopedHeaders != nil || p.scopedSet {
		t.Fatalf("Stop() retained scoped header state: scoped=%v", p.scopedSet)
	}
}

func TestOASStopCancelsOwnedFetchBeforeDroppingSecrets(t *testing.T) {
	const raw = "$ENV://OAS_OWNED_REFRESH_HEADER"
	headerObserved := make(chan string, 1)
	allowHeaderUse := make(chan struct{})
	fetchEntered := make(chan struct{})
	fetchCanceled := make(chan struct{})
	allowFetchReturn := make(chan struct{})
	var releaseHeader sync.Once
	var releaseFetch sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	t.Cleanup(server.Close)

	failures := make(chan runtime.TaskFailure, 1)
	tasks := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
		failures <- failure
	})
	owner, err := runtime.NewTaskOwner(tasks, "plugin/test/oas/ordered-stop", runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{config: Config{
		SpecURL:                   server.URL,
		SpecURLRequestHeaders:     map[string]string{"Authorization": raw},
		SkipRequestBodyValidation: true,
	}}
	p.SetDependencies(base.Dependencies{Tasks: owner})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: "Bearer generation-owned"},
	)
	t.Cleanup(closeAttempt)
	materializeScopedOASSecrets(t, p, secrets, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	registerOASTestTaskState(t, p, tasks, failures)
	t.Cleanup(func() {
		releaseHeader.Do(func() { close(allowHeaderUse) })
		releaseFetch.Do(func() { close(allowFetchReturn) })
	})

	now := time.Unix(100, 0)
	p.now = func() time.Time { return now }
	want, err := p.validator()
	if err != nil {
		t.Fatalf("initial validator() error = %v", err)
	}
	p.refreshCompile = func(ctx context.Context) (*compiledSpec, error) {
		releaseWork, err := p.acquireOASWork()
		if err != nil {
			return nil, err
		}
		defer releaseWork()
		err = p.withRequestHeaders(func(headers map[string]string) error {
			headerObserved <- headers["Authorization"]
			<-allowHeaderUse
			close(fetchEntered)
			<-ctx.Done()
			close(fetchCanceled)
			<-allowFetchReturn
			return ctx.Err()
		})
		return nil, err
	}
	now = now.Add(p.specURLTTL() + time.Second)
	if got, err := p.validator(); err != nil || got != want {
		t.Fatalf("due validator() = (%p, %v), want last-good %p", got, err, want)
	}
	if header := <-headerObserved; header != "Bearer generation-owned" {
		t.Fatalf("refresh Authorization = %q", header)
	}
	releaseHeader.Do(func() { close(allowHeaderUse) })
	<-fetchEntered

	stopped := make(chan struct{})
	go func() {
		stopOASTestPlugin(t, p)
		close(stopped)
	}()
	<-fetchCanceled
	select {
	case <-stopped:
		t.Fatal("plugin stopped before the canceled fetch released its secret use")
	default:
	}
	if !p.scopedSet || len(p.scopedHeaders) != 1 || p.compiled.Load() != want {
		t.Fatalf(
			"state before refresh join = scoped:%v headers:%d compiled:%p, want retained compiled:%p",
			p.scopedSet,
			len(p.scopedHeaders),
			p.compiled.Load(),
			want,
		)
	}

	releaseFetch.Do(func() { close(allowFetchReturn) })
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("generation registry did not join the canceled OAS refresh")
	}
	if p.scopedSet || p.scopedHeaders != nil || p.compiled.Load() != nil {
		t.Fatalf(
			"state after ordered stop = scoped:%v headers:%d compiled:%p",
			p.scopedSet,
			len(p.scopedHeaders),
			p.compiled.Load(),
		)
	}
}

func TestScopedSecretsOASStopDuringMaterializeCannotRevive(t *testing.T) {
	const raw = "$ENV://OAS_STOP_DURING_MATERIALIZE"
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: testSpec()},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call oasScopedSecretCall) {
		if call.Raw == raw {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: Config{Spec: raw}}
	materializeDone := make(chan error, 1)
	go func() {
		materializeDone <- base.MaterializeScopedPluginSecrets(
			context.Background(), scope, secrets, p,
		)
	}()
	<-entered
	p.Stop()
	close(release)
	if err := <-materializeDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("materialization after Stop error = %v", err)
	}
	if p.config.Spec != raw || p.scopedInline != (secret.Value{}) || p.scopedSet {
		t.Fatalf(
			"stopped materialization revived state: config=%#v scoped=%v",
			p.config, p.scopedSet,
		)
	}
	calls := len(broker.callsSnapshot())
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("second materialization error = %v", err)
	}
	if got := len(broker.callsSnapshot()); got != calls {
		t.Fatalf("post-Stop materialization made %d new broker calls", got-calls)
	}
}

func TestScopedSecretsOASConcurrentMaterializationIsSingleFlight(t *testing.T) {
	const raw = "$ENV://OAS_SINGLEFLIGHT_SPEC"
	secrets, scope, broker, closeAttempt := newOASScopedSecretHarness(
		t, map[string]string{raw: testSpec()},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call oasScopedSecretCall) {
		if call.Raw == raw {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: Config{Spec: raw}}
	errs := make(chan error, 16)
	var wait sync.WaitGroup
	wait.Go(func() {
		errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	})
	<-entered
	for range 15 {
		wait.Go(func() {
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
		})
	}
	close(release)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialization error = %v", err)
		}
	}
	assertOASScopedCalls(t, scope, broker.callsSnapshot(), []string{"spec"}, []string{raw})
	p.Stop()
}

func TestOASPostInitAfterStopWithoutSecretsPublishesNothing(t *testing.T) {
	p := &Plugin{config: Config{SpecURL: "https://example.test/openapi.json"}}
	p.Stop()
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want ErrCredentialUnavailable", err)
	}
	if p.config.Timeout != 0 || p.config.RejectionStatusCode != 0 || p.metadata != (Metadata{}) ||
		p.compiled.Load() != nil || p.refreshAdmissionErr != nil {
		t.Fatalf("post-Stop PostInit published state: config=%#v metadata=%#v", p.config, p.metadata)
	}
}

func TestOASStopWaitsForNoSecretCompileAndSuppressesOldResult(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer server.Close()
	p := &Plugin{config: Config{SpecURL: server.URL}}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	validatorDone := make(chan error, 1)
	go func() {
		_, err := p.validator()
		validatorDone <- err
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(release)
		<-validatorDone
		t.Fatal("Stop() returned before no-secret compile completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-validatorDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("old validator result error = %v, want ErrCredentialUnavailable", err)
	}
	<-stopDone
	if p.compiled.Load() != nil {
		t.Fatal("stopped no-secret compile published a validator")
	}
}

func TestOASStopSuppressesValidatorQueuedBehindInitialCompile(t *testing.T) {
	p := &Plugin{}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	validatorDone := make(chan error, 1)
	go func() {
		_, err := p.validator()
		validatorDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		p.lifecycleMu.Lock()
		active := p.activeWork
		p.lifecycleMu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			p.mu.Unlock()
			t.Fatal("validator did not enter lifecycle gate")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	p.compiled.Store(&compiledSpec{})
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	for !p.retired.Load() {
		if time.Now().After(deadline) {
			p.mu.Unlock()
			t.Fatal("Stop did not retire plugin")
		}
		time.Sleep(time.Millisecond)
	}
	p.mu.Unlock()
	if err := <-validatorDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("queued validator error = %v, want ErrCredentialUnavailable", err)
	}
	<-stopDone
}

type blockingOASRequestBody struct {
	entered chan struct{}
	release chan struct{}
	data    []byte
	once    sync.Once
	done    bool
}

func (body *blockingOASRequestBody) Read(target []byte) (int, error) {
	if body.done {
		return 0, io.EOF
	}
	body.once.Do(func() { close(body.entered) })
	<-body.release
	body.done = true
	return copy(target, body.data), nil
}

func (*blockingOASRequestBody) Close() error { return nil }

func TestOASStopWaitsForNoSecretHandlerValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSpec()))
	}))
	defer server.Close()
	p := &Plugin{config: Config{SpecURL: server.URL}}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.validator(); err != nil {
		t.Fatal(err)
	}
	body := &blockingOASRequestBody{
		entered: make(chan struct{}), release: make(chan struct{}),
		data: []byte(`{"name":"doggie"}`),
	}
	request := httptest.NewRequest(http.MethodPost, "/pets/123?verbose=true", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Trace", "trace-id")
	handlerDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(recorder, request)
		handlerDone <- recorder.Code
	}()
	<-body.entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(body.release)
		<-handlerDone
		t.Fatal("Stop() returned before no-secret request validation completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(body.release)
	if code := <-handlerDone; code != http.StatusNoContent {
		t.Fatalf("handler response = %d", code)
	}
	<-stopDone
}
