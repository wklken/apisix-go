package compiler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestWorkerCompilerFactoryClosePublicSurface(t *testing.T) {
	if method, ok := reflect.TypeFor[*WorkerCompilerFactory]().MethodByName("Close"); !ok {
		t.Error("WorkerCompilerFactory is missing Close(context.Context) error")
	} else if method.Type.NumIn() != 2 || method.Type.NumOut() != 1 ||
		method.Type.Out(0) != reflect.TypeFor[error]() {
		t.Fatalf("WorkerCompilerFactory.Close has unexpected shape %v", method.Type)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "types.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, spec := range general.Specs {
			value := spec.(*ast.ValueSpec)
			for _, name := range value.Names {
				found = found || name.Name == "ErrWorkerCompilerFactoryClosed"
			}
		}
	}
	if !found {
		t.Error("types.go is missing ErrWorkerCompilerFactoryClosed")
	}
}

func TestWorkerCompilerFactoryCloseRejectsCandidateDuringCleanup(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 9001, nil, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	cleanupEntered := make(chan struct{})
	allowCleanup := make(chan struct{})
	gateRelocked := false
	liveRelocked := false
	registration := materializer.registrationsSnapshot()[0]
	registration.onClose = func() {
		factory.gate.RLock()
		gateRelocked = true
		factory.gate.RUnlock()
		factory.liveMu.Lock()
		liveRelocked = true
		factory.liveMu.Unlock()
		close(cleanupEntered)
		<-allowCleanup
	}
	var checkpoints atomic.Int64
	factory.checkpoint = func(string, workerFactoryCheckpointState) error {
		checkpoints.Add(1)
		return nil
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- factory.Close(context.Background()) }()
	workerCloseWait(t, cleanupEntered, "factory generation cleanup")
	if !gateRelocked || !liveRelocked {
		t.Fatal("cleanup callback did not reacquire both factory locks")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	rejectedDesired := mustGenerationSnapshot(t, 9002, nil, nil)
	got, candidateErr := factory.PrepareGeneration(
		canceled,
		ticketForSnapshot(rejectedDesired, generation.DomainHTTP),
		rejectedDesired,
		nil,
		nil,
	)
	if got != nil || candidateErr != ErrWorkerCompilerFactoryClosed {
		t.Fatalf(
			"candidate after closed mark = %#v/%v, want exact closed sentinel",
			got,
			candidateErr,
		)
	}
	if len(materializer.registrationsSnapshot()) != 1 || checkpoints.Load() != 0 {
		t.Fatalf("post-close side effects registrations/checkpoint = %d/%d",
			len(materializer.registrationsSnapshot()), checkpoints.Load())
	}
	close(allowCleanup)
	if err := workerCloseWait(t, closeResult, "factory Close"); err != nil {
		t.Fatal(err)
	}
	if prepared.ConsumerLookup() != nil {
		t.Fatal("factory Close left prepared consumer lookup live")
	}
}

func TestWorkerCompilerFactoryCloseUsesStableAttemptOrderAndClosesRegistryLast(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	trace := &workerCloseTrace{}
	type ownedGeneration struct {
		id       secret.AttemptID
		prepared *PreparedGeneration
		label    string
	}
	owned := make([]ownedGeneration, 0, 2)
	for index, revision := range []uint64{9012, 9011} {
		label := fmt.Sprintf("generation-%d", index)
		routeID := fmt.Sprintf("close-route-%d", index)
		desired := mustGenerationSnapshot(t, revision, []generation.Resource{
			resourceValue("routes", routeID, fmt.Sprintf(
				`{"id":%q,"plugins":{"request-id":{}}}`,
				routeID,
			)),
		}, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		id := prepared.attempt.AttemptID()
		prepared.bindingOps.closeStarted = func() { trace.record(label + ":invoke") }
		prepared.bindingOps.trace = func(stage string) { trace.record(label + ":" + stage) }
		for releaseIndex := range prepared.cleanup.releases {
			step := &prepared.cleanup.releases[releaseIndex]
			if step.name != "consumer-bindings" && step.name != "attempt-registration" {
				continue
			}
			name := step.name
			original := step.run
			step.run = func(ctx context.Context) error {
				err := original(ctx)
				trace.record(label + ":" + name)
				return err
			}
		}
		if err := prepared.tasks.Go(
			runtime.TaskSpec{Owner: label, Criticality: runtime.TaskCore},
			func(ctx context.Context) error {
				<-ctx.Done()
				trace.record(label + ":task-exit")
				return nil
			},
		); err != nil {
			t.Fatal(err)
		}
		materializeWorkerCloseRequestID(t, prepared, routeID)
		owned = append(owned, ownedGeneration{id: id, prepared: prepared, label: label})
	}

	registryKey := runtime.ResourceKey{
		Kind: "factory-close-sentinel", Scope: "factory-close", Digest: sha256.Sum256([]byte("registry-last")),
	}
	residualLease, err := runtime.Acquire(
		context.Background(), factory.registry, registryKey,
		func(context.Context) (string, func(context.Context) error, error) {
			return "sentinel", func(context.Context) error {
				trace.record("registry-close")
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(owned, func(left, right ownedGeneration) int {
		return bytes.Compare(left.id[:], right.id[:])
	})
	var want []string
	for _, generationOwner := range owned {
		want = append(want,
			generationOwner.label+":invoke",
			generationOwner.label+":task-exit",
			generationOwner.label+":lease-release:request-id",
			generationOwner.label+":stop:request-id",
			generationOwner.label+":consumer-bindings",
			generationOwner.label+":attempt-registration",
		)
	}
	want = append(want, "registry-close")
	if got := trace.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("factory cleanup order = %v, want %v", got, want)
	}
	if factory.registry.Len() != 0 {
		t.Fatalf("registry resources after Close = %d, want 0", factory.registry.Len())
	}
	factory.liveMu.Lock()
	live := len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 {
		t.Fatalf("live generations after Close = %d, want 0", live)
	}
	registrations := materializer.registrationsSnapshot()
	for _, registration := range registrations {
		if registration.closed != 1 {
			t.Fatalf("registration close count = %d, want 1", registration.closed)
		}
	}
	var postCloseFactories atomic.Int64
	_, acquireErr := runtime.Acquire(
		context.Background(), factory.registry,
		runtime.ResourceKey{
			Kind: "after-close", Scope: "after-close", Digest: sha256.Sum256([]byte("after-close")),
		},
		func(context.Context) (string, func(context.Context) error, error) {
			postCloseFactories.Add(1)
			return "unexpected", nil, nil
		},
	)
	if !errors.Is(acquireErr, runtime.ErrResourceRegistryClosed) || postCloseFactories.Load() != 0 {
		t.Fatalf("post-close Acquire = %v/factory-calls=%d", acquireErr, postCloseFactories.Load())
	}
	if err := residualLease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryCloseResidualRetainsGenerationAndDefersRegistry(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	blockedDesired := mustGenerationSnapshot(t, 9013, nil, nil)
	blocked, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(blockedDesired, generation.DomainHTTP),
		blockedDesired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalDesired := mustGenerationSnapshot(t, 9014, nil, nil)
	terminal, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(terminalDesired, generation.DomainHTTP),
		terminalDesired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if terminal == blocked {
		t.Fatal("fixture returned the same generation twice")
	}
	release := make(chan struct{})
	started := make(chan struct{})
	if err := blocked.tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/factory-blocking", Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	var registryCloses atomic.Int64
	lease := workerFactoryCloseSentinel(t, factory, "deferred-registry", &registryCloses)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- factory.Close(ctx) }()
	var first error
	select {
	case first = <-firstDone:
	case <-time.After(time.Second):
		close(release)
		first = <-firstDone
		t.Fatalf("first factory Close blocked past caller deadline: %v", first)
	}
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) ||
		!errors.Is(first, ErrPreparedGenerationCleanupIncomplete) ||
		!errors.Is(first, errWorkerCompilerFactoryCleanupFailed) {
		t.Fatalf("first factory Close = %v, residual = %#v", first, residual)
	}
	factory.liveMu.Lock()
	tracked := factory.live[blocked.attempt.AttemptID()]
	live := len(factory.live)
	factory.liveMu.Unlock()
	if tracked != blocked || live != 1 {
		t.Fatalf(
			"incomplete generation ownership = tracked:%p live:%d, want %p/1",
			tracked,
			live,
			blocked,
		)
	}
	if registryCloses.Load() != 0 {
		t.Fatalf("registry closed across residual: closes:%d", registryCloses.Load())
	}
	openLease := workerFactoryCloseSentinel(t, factory, "deferred-registry-open", new(atomic.Int64))

	close(release)
	if err := factory.Close(context.Background()); err != nil {
		t.Fatalf("retry factory Close = %v", err)
	}
	if factory.registry.Len() != 0 || registryCloses.Load() != 1 {
		t.Fatalf(
			"terminal registry cleanup = len:%d closes:%d",
			factory.registry.Len(),
			registryCloses.Load(),
		)
	}
	factory.liveMu.Lock()
	live = len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 {
		t.Fatalf("terminal factory live generations = %d, want 0", live)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("post-registry lease replay = %v", err)
	}
	if err := openLease.Release(context.Background()); err != nil {
		t.Fatalf("post-registry open-check lease replay = %v", err)
	}
}

func TestWorkerCompilerFactoryGenerationTaskQuiescerJoinsSafeMarkerAndExactCarrier(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	const owner = "plugin/test/factory-generation-task"
	started := make(chan struct{})
	release := make(chan struct{})
	factory.checkpoint = func(stage string, state workerFactoryCheckpointState) error {
		if stage != "create-task-registry" {
			return nil
		}
		return state.tasks.Go(
			runtime.TaskSpec{Owner: owner, Criticality: runtime.TaskPlugin},
			func(context.Context) error {
				close(started)
				<-release
				return nil
			},
		)
	}
	desired := mustGenerationSnapshot(t, 9015, nil, nil)
	if _, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- factory.Close(ctx) }()
	var first error
	select {
	case first = <-firstDone:
	case <-time.After(time.Second):
		close(release)
		first = <-firstDone
		t.Fatalf("generation-task Close blocked past caller deadline: %v", first)
	}
	var residual *runtime.TaskResidualError
	if !errors.Is(first, errWorkerGenerationCleanupFailed) || !errors.As(first, &residual) {
		t.Fatalf("generation-task Close = %v, residual = %#v", first, residual)
	}
	if got := residual.Residuals(); !slices.Equal(got, []runtime.TaskResidual{{Owner: owner}}) {
		t.Fatalf("generation-task residuals = %v, want owner %q", got, owner)
	}

	close(release)
	if err := factory.Close(context.Background()); err != nil {
		t.Fatalf("generation-task retry Close = %v", err)
	}
}

func TestWorkerCompilerFactoryCloseRetryPreservesIndependentTerminalErrors(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	terminalDesired := mustGenerationSnapshot(t, 9016, nil, nil)
	terminal, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(terminalDesired, generation.DomainHTTP),
		terminalDesired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	blockedDesired := mustGenerationSnapshot(t, 9017, nil, nil)
	blocked, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(blockedDesired, generation.DomainHTTP),
		blockedDesired,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalErr := &workerTestSecretError{text: "terminal-generation-error"}
	registrations := materializer.registrationsSnapshot()
	if len(registrations) < 2 {
		t.Fatalf("registrations = %d, want two generations", len(registrations))
	}
	registrations[0].closeErr = terminalErr
	release := make(chan struct{})
	started := make(chan struct{})
	if err := blocked.tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/independent-terminal", Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- factory.Close(ctx) }()
	var first error
	select {
	case first = <-firstDone:
	case <-time.After(time.Second):
		close(release)
		first = <-firstDone
		t.Fatalf("independent-error Close blocked past caller deadline: %v", first)
	}
	if !errors.Is(first, errWorkerCompilerFactoryCleanupFailed) ||
		!errors.Is(first, ErrPreparedGenerationCleanupIncomplete) {
		t.Fatalf("first independent-error Close = %v", first)
	}

	close(release)
	finalErr := factory.Close(context.Background())
	if !errors.Is(finalErr, errWorkerCompilerFactoryCleanupFailed) {
		t.Fatalf("retry independent-error Close = %v, want terminal factory marker", finalErr)
	}
	if errors.Is(finalErr, context.DeadlineExceeded) {
		t.Fatalf("retry retained transient deadline in terminal result: %v", finalErr)
	}
	if terminal == blocked {
		t.Fatal("fixture returned the same generation twice")
	}
}

func TestWorkerCompilerFactoryConcurrentRetryHasOneAttemptLeader(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 9018, nil, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	if err := prepared.cleanup.Own(cleanupQuiesce, "transient-quiesce", func(context.Context) error {
		if attempts.Add(1) == 1 {
			close(started)
			<-release
			return errors.New("transient quiesce failure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var registryCloses atomic.Int64
	lease := workerFactoryCloseSentinel(t, factory, "concurrent-retry", &registryCloses)
	firstResult := make(chan error, 1)
	go func() { firstResult <- factory.Close(context.Background()) }()
	<-started
	const callers = 12
	results := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			results <- factory.Close(context.Background())
		}()
	}
	ready.Wait()
	close(release)
	first := <-firstResult
	if !errors.Is(first, ErrPreparedGenerationCleanupIncomplete) ||
		!errors.Is(first, errWorkerCompilerFactoryCleanupFailed) {
		t.Fatalf("first concurrent retry result = %v", first)
	}
	for range callers {
		if got := <-results; got != first {
			t.Fatalf("concurrent retry result = %v, want exact attempt result %v", got, first)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("concurrent first attempts = %d, want 1", attempts.Load())
	}
	closeDone := factory.Close(context.Background())
	if closeDone != nil {
		t.Fatalf("retry after incomplete concurrent attempt = %v", closeDone)
	}
	if attempts.Load() != 2 || registryCloses.Load() != 1 {
		t.Fatalf(
			"retry attempts/registry closes = %d/%d, want 2/1",
			attempts.Load(),
			registryCloses.Load(),
		)
	}
	registrations := materializer.registrationsSnapshot()
	if len(registrations) != 1 || registrations[0].closed != 1 {
		t.Fatalf("concurrent generation releases = %#v", registrations)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("concurrent retry lease replay = %v", err)
	}
}

func TestWorkerCompilerFactoryCloseRetriesOrdinaryQuiesceFailureWithoutCachingIt(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 9019, nil, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int64
	if err := prepared.cleanup.Own(cleanupQuiesce, "ordinary-transient", func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("ordinary transient quiesce failure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var registryCloses atomic.Int64
	lease := workerFactoryCloseSentinel(t, factory, "ordinary-quiesce", &registryCloses)

	first := factory.Close(context.Background())
	if !errors.Is(first, ErrPreparedGenerationCleanupIncomplete) ||
		!errors.Is(first, errWorkerCompilerFactoryCleanupFailed) {
		t.Fatalf("ordinary quiesce Close = %v", first)
	}
	factory.liveMu.Lock()
	tracked := factory.live[prepared.attempt.AttemptID()]
	factory.liveMu.Unlock()
	if tracked != prepared || registryCloses.Load() != 0 {
		t.Fatalf("ordinary quiesce retained tracked:%p/closes:%d", tracked, registryCloses.Load())
	}
	openLease := workerFactoryCloseSentinel(t, factory, "ordinary-quiesce-open", new(atomic.Int64))
	if retry := factory.Close(context.Background()); retry != nil {
		t.Fatalf("ordinary quiesce retry = %v", retry)
	}
	if attempts.Load() != 2 || factory.registry.Len() != 0 || registryCloses.Load() != 1 {
		t.Fatalf(
			"ordinary quiesce retry state = attempts:%d len:%d closes:%d",
			attempts.Load(),
			factory.registry.Len(),
			registryCloses.Load(),
		)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("ordinary quiesce lease replay = %v", err)
	}
	if err := openLease.Release(context.Background()); err != nil {
		t.Fatalf("ordinary quiesce open-check lease replay = %v", err)
	}
}

func workerFactoryCloseSentinel(
	t *testing.T,
	factory *WorkerCompilerFactory,
	label string,
	closes *atomic.Int64,
) *runtime.ResourceLease[string] {
	t.Helper()
	key := runtime.ResourceKey{
		Kind: "factory-close-sentinel", Scope: label, Digest: sha256.Sum256([]byte(label)),
	}
	lease, err := runtime.Acquire(
		context.Background(), factory.registry, key,
		func(context.Context) (string, func(context.Context) error, error) {
			return label, func(context.Context) error {
				closes.Add(1)
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("Acquire(%q) error = %v", label, err)
	}
	return lease
}

func TestWorkerCompilerFactoryCloseWaitsForAdmittedPreparation(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	checkpointEntered := make(chan struct{})
	allowCheckpoint := make(chan struct{})
	cleanupEntered := make(chan struct{})
	allowCleanup := make(chan struct{})
	factory.checkpoint = func(stage string, _ workerFactoryCheckpointState) error {
		if stage == "transfer-prepared-generation" {
			close(checkpointEntered)
			<-allowCheckpoint
		}
		return nil
	}
	desired := mustGenerationSnapshot(t, 9021, nil, nil)
	type preparationResult struct {
		prepared *PreparedGeneration
		err      error
	}
	prepareResult := make(chan preparationResult, 1)
	go func() {
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		prepareResult <- preparationResult{prepared: prepared, err: err}
	}()
	workerCloseWait(t, checkpointEntered, "final preparation checkpoint")
	registration := materializer.registrationsSnapshot()[0]
	registration.onClose = func() {
		close(cleanupEntered)
		<-allowCleanup
	}
	closeCallStarted := make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		close(closeCallStarted)
		closeResult <- factory.Close(context.Background())
	}()
	workerCloseWait(t, closeCallStarted, "factory Close call")
	close(allowCheckpoint)
	result := workerCloseWait(t, prepareResult, "admitted preparation result")
	if result.err != nil || result.prepared == nil {
		t.Fatalf("admitted preparation = %#v/%v", result.prepared, result.err)
	}
	workerCloseWait(t, cleanupEntered, "admitted generation cleanup")
	close(allowCleanup)
	if err := workerCloseWait(t, closeResult, "factory Close result"); err != nil {
		t.Fatal(err)
	}
	if registration.closed != 1 {
		t.Fatalf("admitted registration closes = %d, want 1", registration.closed)
	}
	factory.liveMu.Lock()
	live := len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 {
		t.Fatalf("admitted preparation omitted from close snapshot: live=%d", live)
	}
}

func TestWorkerCompilerFactoryCloseWaitsForAdmittedMaterialization(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	const routeID = "factory-close-materialization"
	desired := mustGenerationSnapshot(t, 9031, []generation.Resource{
		resourceValue(
			"routes",
			routeID,
			`{"id":"factory-close-materialization","plugins":{"request-id":{}}}`,
		),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	spec := workerCloseRequestIDSpec(t, prepared, routeID)
	leaseReady := make(chan struct{})
	allowAcquireReturn := make(chan struct{})
	materialized := make(chan struct{})
	closeAttempted := make(chan struct{})
	var cleanupBeforeMaterialized atomic.Bool
	var constructions atomic.Int64
	defaultNew := prepared.bindingOps.newFactoryInstance
	prepared.bindingOps.newFactoryInstance = func(
		factory string,
		dependencies base.Dependencies,
	) (plugin.FactoryInstance, error) {
		constructions.Add(1)
		return defaultNew(factory, dependencies)
	}
	defaultAcquire := prepared.bindingOps.acquire
	prepared.bindingOps.acquire = func(
		ctx context.Context,
		registry *runtime.ResourceRegistry,
		key runtime.ResourceKey,
		create runtime.ResourceFactory[plugin.Binding],
	) (*runtime.ResourceLease[plugin.Binding], error) {
		lease, acquireErr := defaultAcquire(ctx, registry, key, create)
		close(leaseReady)
		<-allowAcquireReturn
		return lease, acquireErr
	}
	prepared.bindingOps.closeStarted = func() { close(closeAttempted) }
	defaultQuiesce := prepared.cleanup.quiescers[0].run
	prepared.cleanup.quiescers[0].run = func(ctx context.Context) error {
		select {
		case <-materialized:
		default:
			cleanupBeforeMaterialized.Store(true)
		}
		return defaultQuiesce(ctx)
	}
	type materializationResult struct {
		bindings []plugin.Binding
		err      error
	}
	materialization := make(chan materializationResult, 1)
	go func() {
		bindings, materializeErr := prepared.materializeEffectiveBindings(
			context.Background(), []effectiveBindingSpec{spec},
		)
		close(materialized)
		materialization <- materializationResult{bindings: bindings, err: materializeErr}
	}()
	workerCloseWait(t, leaseReady, "final lease before adoption")
	closeResult := make(chan error, 1)
	go func() { closeResult <- factory.Close(context.Background()) }()
	workerCloseWait(t, closeAttempted, "generation Close admission")
	close(allowAcquireReturn)
	result := workerCloseWait(t, materialization, "admitted materialization")
	if result.err != nil || len(result.bindings) != 1 {
		t.Fatalf("admitted materialization = %#v/%v", result.bindings, result.err)
	}
	if err := workerCloseWait(t, closeResult, "factory Close after materialization"); err != nil {
		t.Fatal(err)
	}
	if cleanupBeforeMaterialized.Load() || factory.registry.Len() != 0 {
		t.Fatalf(
			"cleanup ordering/leak = early:%t resources:%d",
			cleanupBeforeMaterialized.Load(),
			factory.registry.Len(),
		)
	}
	before := constructions.Load()
	bindings, materializeErr := prepared.materializeEffectiveBindings(
		context.Background(), []effectiveBindingSpec{spec},
	)
	if bindings != nil || !errors.Is(materializeErr, errEffectiveBindingMaterializationFailed) ||
		constructions.Load() != before {
		t.Fatalf("post-close materialization = %#v/%v constructions=%d/%d",
			bindings, materializeErr, constructions.Load(), before)
	}
}

func TestWorkerCompilerFactoryConcurrentCloseDiscardAndFactoryCloseRunOnce(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 9041, nil, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	set := prepared.PublicationSet()
	start := make(chan struct{})
	results := make(chan error, 3)
	go func() { <-start; results <- prepared.Close(context.Background()) }()
	go func() { <-start; results <- prepared.DiscardPrepared(context.Background(), set) }()
	go func() { <-start; results <- factory.Close(context.Background()) }()
	close(start)
	for range 3 {
		if err := workerCloseWait(t, results, "concurrent terminal owner"); err != nil {
			t.Fatal(err)
		}
	}
	registrations := materializer.registrationsSnapshot()
	if len(registrations) != 1 || registrations[0].closed != 1 {
		t.Fatalf("concurrent terminal registration closes = %#v", registrations)
	}
	factory.liveMu.Lock()
	live := len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 {
		t.Fatalf("concurrent terminal live generations = %d", live)
	}
}

func TestWorkerCompilerFactoryCloseUsesLiveMapKeyWhileDetachIsBlocked(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	owners := make([]*PreparedGeneration, 0, 2)
	for _, revision := range []uint64{9051, 9052} {
		desired := mustGenerationSnapshot(t, revision, nil, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, prepared)
	}
	slices.SortFunc(owners, func(left, right *PreparedGeneration) int {
		leftID, rightID := left.attempt.AttemptID(), right.attempt.AttemptID()
		return bytes.Compare(leftID[:], rightID[:])
	})
	smaller, larger := owners[0], owners[1]
	detachEntered := make(chan struct{})
	allowDetach := make(chan struct{})
	originalDetach := larger.detach
	larger.detach = func() {
		close(detachEntered)
		<-allowDetach
		originalDetach()
	}
	explicitResult := make(chan error, 1)
	go func() { explicitResult <- larger.Close(context.Background()) }()
	workerCloseWait(t, detachEntered, "explicit Close blocked detach")
	larger.materializeMu.Lock()
	cleared := larger.attempt.authority == nil
	larger.materializeMu.Unlock()
	if !cleared {
		t.Fatal("blocked detach did not expose cleared mutable attempt")
	}
	smallerInvoked := make(chan struct{})
	smaller.bindingOps.closeStarted = func() { close(smallerInvoked) }
	factoryResult := make(chan error, 1)
	go func() { factoryResult <- factory.Close(context.Background()) }()
	workerCloseWait(t, smallerInvoked, "lexicographically smaller live-map owner")
	select {
	case err := <-factoryResult:
		t.Fatalf("factory Close returned before live generation detached: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowDetach)
	if err := workerCloseWait(t, explicitResult, "explicit Close result"); err != nil {
		t.Fatal(err)
	}
	if err := workerCloseWait(t, factoryResult, "factory Close result"); err != nil {
		t.Fatal(err)
	}
	factory.liveMu.Lock()
	live := len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 {
		t.Fatalf("blocked detach left %d live generations", live)
	}
}

func TestWorkerCompilerFactoryConcurrentCloseCachesSafeResultAndContext(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	providerErr := &workerTestSecretError{text: "factory-close-registration-secret"}
	materializer.closeErr = providerErr
	desired := mustGenerationSnapshot(t, 9061, nil, nil)
	_, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	registration := materializer.registrationsSnapshot()[0]
	registration.closeErr = providerErr
	cleanupEntered := make(chan struct{})
	allowCleanup := make(chan struct{})
	registration.onClose = func() {
		close(cleanupEntered)
		<-allowCleanup
	}
	firstCtx, cancel := context.WithCancel(
		context.WithValue(context.Background(), workerTestContextKey{}, "retained"),
	)
	cancel()
	firstResult := make(chan error, 1)
	go func() { firstResult <- factory.Close(firstCtx) }()
	workerCloseWait(t, cleanupEntered, "first factory cleanup")

	const callers = 16
	type closeResult struct {
		index int
		err   error
	}
	results := make(chan closeResult, callers)
	var nilContext context.Context
	for index := range callers {
		go func() {
			switch index % 3 {
			case 0:
				results <- closeResult{index: index, err: factory.Close(nilContext)}
				return
			case 1:
				ctx, cancelCaller := context.WithCancel(context.Background())
				cancelCaller()
				results <- closeResult{index: index, err: factory.Close(ctx)}
				return
			default:
				ctx, cancelCaller := context.WithDeadline(context.Background(), time.Unix(1, 0))
				defer cancelCaller()
				results <- closeResult{index: index, err: factory.Close(ctx)}
			}
		}()
	}
	close(allowCleanup)
	firstErr := workerCloseWait(t, firstResult, "first factory Close result")
	if !errors.Is(firstErr, errWorkerCompilerFactoryCleanupFailed) ||
		!errors.Is(firstErr, errPreparedGenerationCleanupFailed) {
		t.Fatalf("first factory Close error = %v, want safe retryable markers", firstErr)
	}
	assertWorkerErrorRedacted(t, firstErr, providerErr)
	for range callers {
		result := workerCloseWait(t, results, "concurrent factory Close result")
		if result.index%3 == 0 && result.err != firstErr {
			t.Fatalf(
				"background concurrent factory Close error = %v, want exact replay %v",
				result.err,
				firstErr,
			)
		}
		if result.index%3 == 1 && result.err != firstErr &&
			!errors.Is(result.err, context.Canceled) {
			t.Fatalf(
				"canceled concurrent factory Close error = %v, want caller cancellation or replay %v",
				result.err,
				firstErr,
			)
		}
		if result.index%3 == 2 && result.err != firstErr &&
			!errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf(
				"expired concurrent factory Close error = %v, want caller deadline or replay %v",
				result.err,
				firstErr,
			)
		}
	}
	if registration.closed != 1 || registration.closeCtxErr != nil ||
		registration.closeValue != "retained" {
		t.Fatalf("factory cleanup registration/context = %#v", registration)
	}
	if err := (*WorkerCompilerFactory)(nil).Close(context.Background()); err != nil {
		t.Fatalf("nil factory Close error = %v", err)
	}
}

func TestWorkerCompilerFactoryCloseAllowsEventOnlyFailureCallback(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	failures := make(chan runtime.TaskFailure, 1)
	factory.checkpoint = func(stage string, state workerFactoryCheckpointState) error {
		if stage != "create-task-registry" {
			return nil
		}
		return state.tasks.Go(
			runtime.TaskSpec{Owner: "factory-close/event-only", Criticality: runtime.TaskCore},
			func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("event-only task failure")
			},
		)
	}
	desired := mustGenerationSnapshot(t, 9071, nil, nil)
	_, err := factory.PrepareGeneration(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
		func(failure runtime.TaskFailure) { failures <- failure },
	)
	if err != nil {
		t.Fatal(err)
	}
	closeResult := make(chan error, 1)
	go func() { closeResult <- factory.Close(context.Background()) }()
	failure := workerCloseWait(t, failures, "event-only task failure")
	if failure.Owner != "factory-close/event-only" || failure.Err == nil {
		t.Fatalf("failure event = %#v", failure)
	}
	if err := workerCloseWait(t, closeResult, "Close after event-only callback"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryCloseRedactsPreparedAndFactoryCleanupErrors(t *testing.T) {
	t.Run("direct prepared close and discard", func(t *testing.T) {
		factory, materializer := newWorkerTestFactory(t)
		registrationErr := &workerTestSecretError{text: "prepared-registration-secret"}
		resourceErr := &workerTestSecretError{text: "prepared-resource-secret"}
		materializer.closeErr = registrationErr
		const routeID = "prepared-redaction"
		desired := mustGenerationSnapshot(t, 9081, []generation.Resource{
			resourceValue(
				"routes",
				routeID,
				`{"id":"prepared-redaction","plugins":{"request-id":{}}}`,
			),
		}, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		materializer.registrationsSnapshot()[0].closeErr = registrationErr
		defaultRelease := prepared.bindingOps.releaseLease
		prepared.bindingOps.releaseLease = func(
			lease *runtime.ResourceLease[plugin.Binding],
			ctx context.Context,
		) error {
			_ = defaultRelease(lease, ctx)
			return resourceErr
		}
		materializeWorkerCloseRequestID(t, prepared, routeID)
		set := prepared.PublicationSet()
		closeErr := prepared.Close(context.Background())
		if closeErr != errPreparedGenerationCleanupFailed {
			t.Fatalf("PreparedGeneration.Close error = %v, want safe marker", closeErr)
		}
		assertWorkerErrorRedacted(t, closeErr, registrationErr, resourceErr)
		if discardErr := prepared.DiscardPrepared(context.Background(), set); discardErr != closeErr {
			t.Fatalf("DiscardPrepared error = %v, want replay %v", discardErr, closeErr)
		}
		if err := factory.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("factory generation and registry cleanup", func(t *testing.T) {
		factory, materializer := newWorkerTestFactory(t)
		registrationErr := &workerTestSecretError{text: "factory-registration-secret"}
		resourceErr := &workerTestSecretError{text: "factory-resource-secret"}
		registryErr := &workerTestSecretError{text: "factory-registry-secret"}
		materializer.closeErr = registrationErr
		const routeID = "factory-redaction"
		desired := mustGenerationSnapshot(t, 9082, []generation.Resource{
			resourceValue(
				"routes",
				routeID,
				`{"id":"factory-redaction","plugins":{"request-id":{}}}`,
			),
		}, nil)
		prepared, err := factory.PrepareGeneration(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		materializer.registrationsSnapshot()[0].closeErr = registrationErr
		defaultRelease := prepared.bindingOps.releaseLease
		prepared.bindingOps.releaseLease = func(
			lease *runtime.ResourceLease[plugin.Binding],
			ctx context.Context,
		) error {
			_ = defaultRelease(lease, ctx)
			return resourceErr
		}
		materializeWorkerCloseRequestID(t, prepared, routeID)
		_, err = runtime.Acquire(
			context.Background(), factory.registry,
			runtime.ResourceKey{
				Kind: "registry-error", Scope: "factory-close", Digest: sha256.Sum256([]byte("registry-error")),
			},
			func(context.Context) (string, func(context.Context) error, error) {
				return "registry-error", func(context.Context) error { return registryErr }, nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		closeErr := factory.Close(context.Background())
		if !errors.Is(closeErr, errWorkerCompilerFactoryCleanupFailed) ||
			!errors.Is(closeErr, errPreparedGenerationCleanupFailed) {
			t.Fatalf("WorkerCompilerFactory.Close error = %v, want safe markers", closeErr)
		}
		assertWorkerErrorRedacted(t, closeErr, registrationErr, resourceErr, registryErr)
		if replayed := factory.Close(context.Background()); replayed != closeErr {
			t.Fatalf("factory Close replay = %v, want %v", replayed, closeErr)
		}
	})
}

func TestWorkerCompilerFactoryCloseRedactsMixedRegistryResidual(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	secretErr := &workerTestSecretError{text: "mixed-registry-secret"}
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/mixed-registry-residual", Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	acquire := func(label string, closeResource func(context.Context) error) {
		t.Helper()
		key := runtime.ResourceKey{
			Kind: "mixed-registry-residual", Scope: label, Digest: sha256.Sum256([]byte(label)),
		}
		if _, err := runtime.Acquire(
			context.Background(), factory.registry, key,
			func(context.Context) (string, func(context.Context) error, error) {
				return label, closeResource, nil
			},
		); err != nil {
			t.Fatalf("Acquire(%q) error = %v", label, err)
		}
	}
	acquire("terminal-secret", func(context.Context) error { return secretErr })
	acquire("task-residual", func(ctx context.Context) error {
		_, stopErr := tasks.Stop(ctx)
		return stopErr
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	closeErr := factory.Close(ctx)
	var residual *runtime.TaskResidualError
	if !errors.Is(closeErr, errWorkerCompilerFactoryCleanupFailed) ||
		!errors.Is(closeErr, context.DeadlineExceeded) || !errors.As(closeErr, &residual) {
		t.Fatalf("mixed registry Close = %v, residual = %#v", closeErr, residual)
	}
	assertWorkerErrorRedacted(t, closeErr, secretErr)

	close(release)
	if _, stopErr := tasks.Stop(context.Background()); stopErr != nil {
		t.Fatalf("TaskRegistry.Stop retry error = %v", stopErr)
	}
}

func workerCloseRequestIDSpec(
	t *testing.T,
	prepared *PreparedGeneration,
	routeID string,
) effectiveBindingSpec {
	t.Helper()
	for _, occurrence := range prepared.attempt.Occurrences(capability.SecretPluginConfig) {
		if occurrence.Factory() != "request-id" || occurrence.Resource() != (generation.ResourceKey{
			Kind: "routes", ID: routeID,
		}) {
			continue
		}
		scope, provenance, ok := effectivePluginSourceIdentity(occurrence.Resource())
		if !ok {
			t.Fatalf("unsupported request-id resource %#v", occurrence.Resource())
		}
		return effectiveBindingSpec{
			domain:         generation.DomainHTTP,
			executionOwner: occurrence.Resource(),
			source: effectiveBindingSource{
				kind: effectiveBindingPluginConfig, resource: occurrence.Resource(),
				source: capability.SecretPluginConfig, occurrence: occurrence,
			},
			factory:    "request-id",
			config:     resource.PluginConfig(map[string]any{}),
			scope:      scope,
			provenance: provenance,
			resourceContext: effectiveBindingResourceContext{
				kind: effectiveBindingContextHTTP, route: resource.Route{ID: routeID},
			},
		}
	}
	t.Fatal("prepared generation has no exact request-id occurrence")
	return effectiveBindingSpec{}
}

func materializeWorkerCloseRequestID(
	t *testing.T,
	prepared *PreparedGeneration,
	routeID string,
) plugin.Binding {
	t.Helper()
	bindings, err := prepared.materializeEffectiveBindings(
		context.Background(),
		[]effectiveBindingSpec{workerCloseRequestIDSpec(t, prepared, routeID)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("materialized bindings = %d, want 1", len(bindings))
	}
	return bindings[0]
}

type workerCloseTrace struct {
	mu     sync.Mutex
	values []string
}

func (trace *workerCloseTrace) record(value string) {
	trace.mu.Lock()
	trace.values = append(trace.values, value)
	trace.mu.Unlock()
}

func (trace *workerCloseTrace) snapshot() []string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return slices.Clone(trace.values)
}

func workerCloseWait[T any](t *testing.T, values <-chan T, operation string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		var zero T
		return zero
	}
}
