package compiler

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestPreparedGenerationPublicationSetIsDefensive(t *testing.T) {
	prepared, _, _ := preparedGenerationFixture(t)
	want := preparedGenerationPublicationSet(t)

	first := prepared.PublicationSet()
	resources := first.Domains[generation.DomainHTTP].Snapshot.Resources()
	resources[0].Value[0] = 'x'
	candidate := first.Domains[generation.DomainHTTP]
	candidate.Closure[0].ID = "mutated-closure"
	candidate.Decisions[0].Code = "mutated-decision"
	first.Domains[generation.DomainHTTP] = candidate
	delete(first.Domains, generation.DomainStream)
	first.Domains["mutated"] = generation.PublicationCandidate{}

	if got := prepared.PublicationSet(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PublicationSet() after caller mutation = %#v, want %#v", got, want)
	}
}

func TestPreparedGenerationMetadataViewIsDefensive(t *testing.T) {
	prepared, _, _ := preparedGenerationFixture(t)

	var first map[string]any
	if ok, err := prepared.MetadataView().Decode("logger", &first); err != nil || !ok {
		t.Fatalf("first MetadataView().Decode() = (%v, %v)", ok, err)
	}
	nested := first["nested"].(map[string]any)
	nested["owner"] = "mutated"
	first["added"] = true

	var second map[string]any
	if ok, err := prepared.MetadataView().Decode("logger", &second); err != nil || !ok {
		t.Fatalf("second MetadataView().Decode() = (%v, %v)", ok, err)
	}
	want := map[string]any{"nested": map[string]any{"owner": "original"}}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("MetadataView().Decode() after caller mutation = %#v, want %#v", second, want)
	}
}

func TestPreparedGenerationConsumerLookupExposesNoClose(t *testing.T) {
	prepared, _, _ := preparedGenerationFixture(t)

	lookup := prepared.ConsumerLookup()
	if lookup == nil {
		t.Fatal("ConsumerLookup() = nil before close")
	}
	if _, exposesClose := lookup.(interface{ Close() }); exposesClose {
		t.Fatalf("ConsumerLookup() type %T exposes Close", lookup)
	}
	consumer, ok := lookup.ConsumerByID("consumer-1")
	if !ok || consumer.Username != "original" {
		t.Fatalf("ConsumerByID() = (%+v, %v)", consumer, ok)
	}
	consumer.Username = "mutated"
	consumer.Plugins["key-auth"].(map[string]any)["key"] = "mutated"
	consumer, ok = prepared.ConsumerLookup().ConsumerByID("consumer-1")
	if !ok {
		t.Fatal("ConsumerByID() did not find consumer after caller mutation")
	}
	keyAuth, ok := consumer.Plugins["key-auth"].(map[string]any)
	if !ok || consumer.Username != "original" || keyAuth["key"] != "secret" {
		t.Fatalf("ConsumerByID() after caller mutation = (%+v, %v)", consumer, ok)
	}
}

func TestPreparedGenerationAccessorsAreInertAfterClose(t *testing.T) {
	prepared, _, _ := preparedGenerationFixture(t)
	previousLookup := prepared.ConsumerLookup()

	if err := prepared.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := prepared.PublicationSet(); !reflect.DeepEqual(got, generation.PublicationSet{}) {
		t.Fatalf("PublicationSet() after Close = %#v, want zero", got)
	}
	var metadata map[string]any
	if ok, err := prepared.MetadataView().Decode("logger", &metadata); err != nil || ok {
		t.Fatalf("MetadataView().Decode() after Close = (%v, %v), want (false, nil)", ok, err)
	}
	if lookup := prepared.ConsumerLookup(); lookup != nil {
		if _, ok := lookup.ConsumerByID("consumer-1"); ok {
			t.Fatal("ConsumerLookup() returned a live consumer after Close")
		}
	}
	if _, ok := previousLookup.ConsumerByID("consumer-1"); ok {
		t.Fatal("previously returned ConsumerLookup remained live after Close")
	}
}

func TestPreparedGenerationExactDiscardClosesOnce(t *testing.T) {
	prepared, cleanupCalls, detachCalls := preparedGenerationFixture(t)
	set := prepared.PublicationSet()

	for range 3 {
		if err := prepared.DiscardPrepared(context.Background(), set); err != nil {
			t.Fatalf("DiscardPrepared() error = %v", err)
		}
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
	if got := detachCalls.Load(); got != 1 {
		t.Fatalf("detach calls = %d, want 1", got)
	}
}

func TestPreparedGenerationMismatchedDiscardLeavesOwnersLive(t *testing.T) {
	original := preparedGenerationPublicationSet(t)
	prepared, cleanupCalls, _ := preparedGenerationFixture(t)
	otherSnapshot := preparedGenerationSnapshot(t, 101, "other")
	otherRevisionSnapshot := preparedGenerationSnapshot(t, 102, "payload")
	otherDomainCandidate := clonePublicationCandidateForPreparation(original.Domains[generation.DomainStream])

	tests := []struct {
		name   string
		mutate func(*generation.PublicationSet)
	}{
		{name: "desired revision", mutate: func(set *generation.PublicationSet) { set.DesiredRevision++ }},
		{name: "domain membership", mutate: func(set *generation.PublicationSet) {
			delete(set.Domains, generation.DomainStream)
		}},
		{name: "domain key", mutate: func(set *generation.PublicationSet) {
			delete(set.Domains, generation.DomainStream)
			set.Domains["other"] = otherDomainCandidate
		}},
		{name: "artifact domain", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Artifact.Domain = generation.DomainStream
		})},
		{name: "artifact revision", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Artifact.Revision++
		})},
		{name: "artifact digest", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Artifact.Digest[0]++
		})},
		{
			name: "artifact snapshot identity",
			mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Artifact.Snapshot = "sha256:other"
			}),
		},
		{name: "snapshot revision", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Snapshot = otherRevisionSnapshot
		})},
		{
			name: "snapshot resources and digest",
			mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Snapshot = otherSnapshot
			}),
		},
		{name: "closure key kind", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Closure[0].Kind = "services"
		})},
		{name: "closure key id", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Closure[0].ID = "other"
		})},
		{name: "decision key kind", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Decisions[0].Key.Kind = "services"
		})},
		{name: "decision key id", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Decisions[0].Key.ID = "other"
		})},
		{
			name: "decision disposition",
			mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
				candidate.Decisions[0].Disposition = generation.DispositionLastGood
			}),
		},
		{name: "decision code", mutate: mutatePreparedCandidate(func(candidate *generation.PublicationCandidate) {
			candidate.Decisions[0].Code = "other"
		})},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mismatched := clonePublicationSetForPreparation(original)
			test.mutate(&mismatched)
			err := prepared.DiscardPrepared(context.Background(), mismatched)
			if !errors.Is(err, ErrPreparedSetMismatch) {
				t.Fatalf("DiscardPrepared() error = %v, want %v", err, ErrPreparedSetMismatch)
			}
			if got := cleanupCalls.Load(); got != 0 {
				t.Fatalf("cleanup calls after mismatched discard = %d, want 0", got)
			}
			if _, ok := prepared.ConsumerLookup().ConsumerByID("consumer-1"); !ok {
				t.Fatal("mismatched discard made consumer ownership inert")
			}
		})
	}

	if err := prepared.DiscardPrepared(context.Background(), original); err != nil {
		t.Fatalf("exact DiscardPrepared() error = %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls after exact discard = %d, want 1", got)
	}
}

func TestPreparedGenerationConcurrentExactDiscardAndCloseRunsCleanupOnce(t *testing.T) {
	prepared, cleanupCalls, detachCalls := preparedGenerationFixture(t)
	set := prepared.PublicationSet()

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for index := range callers {
		go func() {
			ready.Done()
			<-start
			if index%2 == 0 {
				errs <- prepared.Close(context.Background())
				return
			}
			errs <- prepared.DiscardPrepared(context.Background(), set)
		}()
	}
	ready.Wait()
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent close/discard error = %v", err)
		}
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}
	if got := detachCalls.Load(); got != 1 {
		t.Fatalf("detach calls = %d, want 1", got)
	}
}

func TestPreparedGenerationCanceledCloseQuiescesTasksBeforeReleaseAndReplaysError(t *testing.T) {
	prepared, _, detachCalls := preparedGenerationFixture(t)
	set := prepared.PublicationSet()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	prepared.tasks = tasks

	taskCanceled := make(chan struct{})
	allowTaskExit := make(chan struct{})
	taskExited := make(chan struct{})
	if err := tasks.Go(runtime.TaskSpec{
		Owner:       "prepared-generation/canceled-close",
		Criticality: runtime.TaskCore,
	}, func(ctx context.Context) error {
		close(taskCanceled)
		<-ctx.Done()
		<-allowTaskExit
		close(taskExited)
		return nil
	}); err != nil {
		t.Fatalf("TaskRegistry.Go() error = %v", err)
	}

	quiesceContext := make(chan context.Context, 1)
	continueQuiesce := make(chan struct{})
	if err := prepared.cleanup.Own(cleanupQuiesce, "tasks", func(ctx context.Context) error {
		quiesceContext <- ctx
		<-continueQuiesce
		residuals, err := tasks.Stop(ctx)
		if len(residuals) != 0 {
			return errors.Join(err, errors.New("task registry retained residuals"))
		}
		return err
	}); err != nil {
		t.Fatalf("Own(tasks) error = %v", err)
	}

	releaseErr := errors.New("sentinel release failure")
	releaseEntered := make(chan struct{})
	allowReleaseReturn := make(chan struct{})
	var releasedBeforeTaskExit atomic.Bool
	if err := prepared.cleanup.Own(cleanupRelease, "sentinel", func(context.Context) error {
		select {
		case <-taskExited:
		default:
			releasedBeforeTaskExit.Store(true)
		}
		close(releaseEntered)
		<-allowReleaseReturn
		return releaseErr
	}); err != nil {
		t.Fatalf("Own(sentinel) error = %v", err)
	}

	type cleanupContextKey struct{}
	callerCtx, cancel := context.WithCancel(
		context.WithValue(context.Background(), cleanupContextKey{}, "caller-value"),
	)
	cancel()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- prepared.Close(callerCtx)
	}()

	cleanupCtx := waitPreparedGenerationSignal(t, quiesceContext, "cleanup context")
	cancelLeaked := cleanupCtx.Err() != nil || cleanupCtx.Done() != nil
	if value := cleanupCtx.Value(cleanupContextKey{}); value != "caller-value" {
		t.Errorf("cleanup context value = %v, want caller-value", value)
	}
	close(continueQuiesce)
	waitPreparedGenerationSignal(t, taskCanceled, "task cancellation")

	const concurrentCallers = 8
	concurrentErrs := make(chan error, concurrentCallers)
	for range concurrentCallers {
		go func() {
			concurrentErrs <- prepared.Close(context.Background())
		}()
	}

	if cancelLeaked {
		waitPreparedGenerationSignal(t, releaseEntered, "premature release")
	} else {
		select {
		case <-releaseEntered:
			releasedBeforeTaskExit.Store(true)
		default:
		}
	}
	close(allowTaskExit)
	waitPreparedGenerationSignal(t, taskExited, "task exit")
	if !cancelLeaked {
		waitPreparedGenerationSignal(t, releaseEntered, "release after task exit")
	}
	close(allowReleaseReturn)

	firstErr := waitPreparedGenerationSignal(t, firstDone, "first Close result")
	if cancelLeaked {
		t.Error("cleanup inherited caller cancellation")
	}
	if releasedBeforeTaskExit.Load() {
		t.Error("release callback ran before task exit")
	}
	if errors.Is(firstErr, context.Canceled) {
		t.Fatalf("first Close() error = %v, cleanup inherited caller cancellation", firstErr)
	}
	assertWorkerErrorRedacted(t, firstErr, releaseErr)
	for range concurrentCallers {
		if err := <-concurrentErrs; err != firstErr {
			t.Fatalf("concurrent Close() error = %v, want replayed %v", err, firstErr)
		}
	}
	if err := prepared.Close(context.Background()); err != firstErr {
		t.Fatalf("repeated Close() error = %v, want replayed %v", err, firstErr)
	}
	if err := prepared.DiscardPrepared(context.Background(), set); err != firstErr {
		t.Fatalf("exact DiscardPrepared() error = %v, want replayed %v", err, firstErr)
	}
	if got := detachCalls.Load(); got != 1 {
		t.Fatalf("detach calls = %d, want 1", got)
	}
}

func TestPreparedGenerationCloseAcceptsNilContext(t *testing.T) {
	prepared, _, _ := preparedGenerationFixture(t)
	var nilCtx context.Context
	if err := prepared.Close(nilCtx); err != nil {
		t.Fatalf("Close(nil) error = %v", err)
	}
}

func TestPreparedGenerationPublicAPIExposesNoRuntimeHandles(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "prepared_generation.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("ParseFile(prepared_generation.go) error = %v", err)
	}

	allowedMethods := map[string]struct{}{
		"PublicationSet":  {},
		"MetadataView":    {},
		"ConsumerLookup":  {},
		"HTTP":            {},
		"DiscardPrepared": {},
		"Close":           {},
	}
	bannedNames := map[string]struct{}{
		"PreparedBindingView": {}, "BindingView": {}, "PluginBinding": {},
		"Bindings": {}, "Plugins": {}, "Leases": {}, "Resources": {}, "Tasks": {},
		"AttemptRegistration": {}, "GenerationCapability": {}, "Materializer": {},
		"TaskRegistry": {}, "ResourceRegistry": {}, "ResourceLease": {},
		"ConsumerBindings": {}, "Plugin": {}, "Binding": {}, "FactoryInstance": {},
		"Store": {}, "Resolver": {},
	}
	seenMethods := make(map[string]struct{}, len(allowedMethods))
	seenPreparedType := false
	for _, declaration := range file.Decls {
		switch declaration := declaration.(type) {
		case *ast.GenDecl:
			if declaration.Tok != token.TYPE {
				continue
			}
			for _, spec := range declaration.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if typeSpec.Name.IsExported() && typeSpec.Name.Name != "PreparedGeneration" {
					t.Fatalf("prepared_generation.go exports unexpected type %s", typeSpec.Name.Name)
				}
				if typeSpec.Name.Name != "PreparedGeneration" {
					continue
				}
				seenPreparedType = true
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					t.Fatal("PreparedGeneration is not a struct")
				}
				for _, field := range structure.Fields.List {
					if len(field.Names) == 0 {
						t.Fatal("PreparedGeneration has an embedded field that can promote runtime handles")
					}
					for _, name := range field.Names {
						if name.IsExported() {
							t.Fatalf("PreparedGeneration exports field %s", name.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			if declaration.Recv == nil {
				if declaration.Name.IsExported() {
					t.Fatalf("prepared_generation.go exports unexpected function %s", declaration.Name.Name)
				}
				continue
			}
			if !preparedGenerationReceiver(declaration.Recv.List[0].Type) || !declaration.Name.IsExported() {
				continue
			}
			if _, ok := allowedMethods[declaration.Name.Name]; !ok {
				t.Fatalf("PreparedGeneration exports unexpected method %s", declaration.Name.Name)
			}
			seenMethods[declaration.Name.Name] = struct{}{}
			ast.Inspect(declaration.Type, func(node ast.Node) bool {
				identifier, ok := node.(*ast.Ident)
				if ok {
					if _, banned := bannedNames[identifier.Name]; banned {
						t.Errorf(
							"PreparedGeneration.%s exposes banned type/name %s",
							declaration.Name.Name,
							identifier.Name,
						)
					}
				}
				return true
			})
		}
	}
	if !seenPreparedType {
		t.Fatal("prepared_generation.go does not define PreparedGeneration")
	}
	if !reflect.DeepEqual(seenMethods, allowedMethods) {
		t.Fatalf("PreparedGeneration exported AST methods = %v, want %v", seenMethods, allowedMethods)
	}
	assertPreparedGenerationMethodSet(t)
}

func preparedGenerationFixture(t *testing.T) (*PreparedGeneration, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	metadata, err := runtime.NewMetadataView(map[string][]byte{
		"logger": []byte(`{"nested":{"owner":"original"}}`),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	consumers, err := runtime.NewConsumerBindings(
		[]runtime.ConsumerRecord{{
			ID: "consumer-1",
			Consumer: resource.Consumer{
				Username: "original",
				Plugins: map[string]resource.PluginConfig{
					"key-auth": map[string]any{"key": "secret"},
				},
			},
		}},
		nil,
		[]runtime.ConsumerCredentialBinding{{Plugin: "key-auth", Key: "secret", ConsumerID: "consumer-1"}},
	)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	cleanupCalls := &atomic.Int64{}
	detachCalls := &atomic.Int64{}
	cleanup := &cleanupStack{}
	if err := cleanup.Own(cleanupRelease, "fixture", func(context.Context) error {
		cleanupCalls.Add(1)
		consumers.Close()
		return nil
	}); err != nil {
		t.Fatalf("Own(fixture) error = %v", err)
	}
	return &PreparedGeneration{
		publication: preparedGenerationPublicationSet(t),
		metadata:    metadata,
		consumers:   consumers,
		lookup:      consumerLookupView{bindings: consumers},
		cleanup:     cleanup,
		detach:      func() { detachCalls.Add(1) },
	}, cleanupCalls, detachCalls
}

func preparedGenerationPublicationSet(t *testing.T) generation.PublicationSet {
	t.Helper()
	httpSnapshot := preparedGenerationSnapshot(t, 101, "payload")
	streamSnapshot := preparedGenerationSnapshot(t, 201, "stream")
	return generation.PublicationSet{
		DesiredRevision: 301,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {
				Artifact: generation.GenerationArtifact{
					Domain: generation.DomainHTTP, Revision: 101, Digest: httpSnapshot.Digest(),
					Snapshot: httpSnapshot.SnapshotID(),
				},
				Snapshot: httpSnapshot,
				Closure:  []generation.ResourceKey{{Kind: "routes", ID: "route-1"}},
				Decisions: []generation.ResourceDecision{{
					Key:         generation.ResourceKey{Kind: "routes", ID: "route-1"},
					Disposition: generation.DispositionPublished,
					Code:        "compiled",
				}},
			},
			generation.DomainStream: {
				Artifact: generation.GenerationArtifact{
					Domain: generation.DomainStream, Revision: 201, Digest: streamSnapshot.Digest(),
					Snapshot: streamSnapshot.SnapshotID(),
				},
				Snapshot: streamSnapshot,
				Closure:  []generation.ResourceKey{{Kind: "stream_routes", ID: "stream-1"}},
				Decisions: []generation.ResourceDecision{{
					Key:         generation.ResourceKey{Kind: "stream_routes", ID: "stream-1"},
					Disposition: generation.DispositionPublished,
					Code:        "compiled",
				}},
			},
		},
	}
}

func preparedGenerationSnapshot(t *testing.T, revision uint64, value string) generation.Snapshot {
	t.Helper()
	snapshot, err := generation.NewSnapshot(
		revision,
		[]generation.Resource{{
			Key:   generation.ResourceKey{Kind: "routes", ID: "route-1"},
			Value: []byte(value),
		}},
		[]generation.Tombstone{{
			Key:      generation.ResourceKey{Kind: "services", ID: "deleted-1"},
			Revision: revision,
		}},
	)
	if err != nil {
		t.Fatalf("NewSnapshot() error = %v", err)
	}
	return snapshot
}

func mutatePreparedCandidate(
	mutate func(*generation.PublicationCandidate),
) func(*generation.PublicationSet) {
	return func(set *generation.PublicationSet) {
		candidate := set.Domains[generation.DomainHTTP]
		mutate(&candidate)
		set.Domains[generation.DomainHTTP] = candidate
	}
}

func preparedGenerationReceiver(expression ast.Expr) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == "PreparedGeneration"
}

func assertPreparedGenerationMethodSet(t *testing.T) {
	t.Helper()
	preparedType := reflect.TypeFor[*PreparedGeneration]()
	contextType := reflect.TypeFor[context.Context]()
	publicationType := reflect.TypeFor[generation.PublicationSet]()
	metadataType := reflect.TypeFor[runtime.MetadataView]()
	consumerLookupType := reflect.TypeFor[base.ConsumerLookup]()
	httpSnapshotType := reflect.TypeFor[*HTTPSnapshot]()
	errorType := reflect.TypeFor[error]()
	expected := map[string]struct {
		inputs  []reflect.Type
		outputs []reflect.Type
	}{
		"PublicationSet": {outputs: []reflect.Type{publicationType}},
		"MetadataView":   {outputs: []reflect.Type{metadataType}},
		"ConsumerLookup": {outputs: []reflect.Type{consumerLookupType}},
		"HTTP":           {outputs: []reflect.Type{httpSnapshotType}},
		"DiscardPrepared": {
			inputs: []reflect.Type{contextType, publicationType}, outputs: []reflect.Type{errorType},
		},
		"Close": {inputs: []reflect.Type{contextType}, outputs: []reflect.Type{errorType}},
	}
	if preparedType.NumMethod() != len(expected) {
		t.Fatalf("PreparedGeneration exported method count = %d, want %d", preparedType.NumMethod(), len(expected))
	}
	for name, signature := range expected {
		method, ok := preparedType.MethodByName(name)
		if !ok {
			t.Errorf("PreparedGeneration is missing exported method %s", name)
			continue
		}
		if method.Type.NumIn() != len(signature.inputs)+1 || method.Type.In(0) != preparedType {
			t.Errorf("PreparedGeneration.%s input count/receiver = %v", name, method.Type)
			continue
		}
		for index, input := range signature.inputs {
			if method.Type.In(index+1) != input {
				t.Errorf("PreparedGeneration.%s input %d = %v, want %v", name, index, method.Type.In(index+1), input)
			}
		}
		if method.Type.NumOut() != len(signature.outputs) {
			t.Errorf(
				"PreparedGeneration.%s output count = %d, want %d",
				name,
				method.Type.NumOut(),
				len(signature.outputs),
			)
			continue
		}
		for index, output := range signature.outputs {
			if method.Type.Out(index) != output {
				t.Errorf("PreparedGeneration.%s output %d = %v, want %v", name, index, method.Type.Out(index), output)
			}
		}
	}
}

func waitPreparedGenerationSignal[T any](t *testing.T, signal <-chan T, name string) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}
