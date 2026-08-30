package compiler

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type countingScopedBroker struct {
	resolveCalls int
	resolveErr   error
}

func (broker *countingScopedBroker) ResolveScoped(
	context.Context,
	secret.Scope,
	string,
) (string, error) {
	broker.resolveCalls++
	if broker.resolveErr != nil {
		return "", broker.resolveErr
	}
	return "resolved", nil
}

type countingMaterializer struct {
	delegate     secret.Materializer
	trace        *[]string
	prepareCalls int
	lastSet      generation.PublicationSet
	last         secret.GenerationMaterialization
}

func (materializer *countingMaterializer) PrepareGeneration(
	ctx context.Context,
	set generation.PublicationSet,
) (secret.GenerationMaterialization, error) {
	materializer.prepareCalls++
	if materializer.trace != nil {
		*materializer.trace = append(*materializer.trace, "prepare-generation")
	}
	materializer.lastSet = clonePublicationSetForPreparation(set)
	owner, err := materializer.delegate.PrepareGeneration(ctx, set)
	materializer.last = owner
	return owner, err
}

func (materializer *countingMaterializer) DeclarationDigest() [32]byte {
	return materializer.delegate.DeclarationDigest()
}

type resultMaterializer struct {
	digest [32]byte
	owner  secret.GenerationMaterialization
	err    error
	calls  int
}

func (materializer *resultMaterializer) PrepareGeneration(
	context.Context,
	generation.PublicationSet,
) (secret.GenerationMaterialization, error) {
	materializer.calls++
	return materializer.owner, materializer.err
}

func (materializer *resultMaterializer) DeclarationDigest() [32]byte { return materializer.digest }

type recordingGenerationMaterialization struct {
	delegate   secret.GenerationMaterialization
	closeErr   error
	closeCalls int
	closeCtx   error
	mu         sync.Mutex
}

func (owner *recordingGenerationMaterialization) Secrets() secret.GenerationSecrets {
	if owner == nil || owner.delegate == nil {
		return secret.GenerationSecrets{}
	}
	return owner.delegate.Secrets()
}

func (owner *recordingGenerationMaterialization) Close(ctx context.Context) error {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.closeCalls++
	owner.closeCtx = ctx.Err()
	if owner.delegate != nil {
		_ = owner.delegate.Close(ctx)
	}
	return owner.closeErr
}

func TestGenerationFactoryPreparesExactFinalPublication(t *testing.T) {
	factory, materializer, trace := newScopedGenerationFactory(t, &countingScopedBroker{})
	desired := mustGenerationSnapshot(t, 41, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	want, err := factory.compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := factory.prepareGenerationSecrets(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	if !reflect.DeepEqual(materializer.lastSet, want) {
		t.Fatalf("materialized set = %#v, want %#v", materializer.lastSet, want)
	}
	if !reflect.DeepEqual(prepared.publication, want) {
		t.Fatalf("owned publication = %#v, want %#v", prepared.publication, want)
	}
	if prepared.preparation.Generation() != desired.Revision() || materializer.prepareCalls != 1 {
		t.Fatalf("prepared generation/calls = %d/%d", prepared.preparation.Generation(), materializer.prepareCalls)
	}
	if got := *trace; !reflect.DeepEqual(got, []string{"prepare-generation"}) {
		t.Fatalf("side-effect trace = %v", got)
	}
}

func TestGenerationFactoryRejectsInvalidDesiredBeforeMaterialization(t *testing.T) {
	factory, materializer, _ := newScopedGenerationFactory(t, &countingScopedBroker{})
	desired := mustGenerationSnapshot(t, 42, []generation.Resource{
		resourceValue("routes", "broken", `{"id":"broken","plugins":{"unknown-plugin":{}}}`),
	}, nil)
	if _, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	); err == nil {
		t.Fatal("prepare invalid desired error = nil")
	}
	if materializer.prepareCalls != 0 {
		t.Fatalf("materializer calls = %d, want zero", materializer.prepareCalls)
	}
}

func TestGenerationFactoryClosesOwnerReturnedWithError(t *testing.T) {
	compiler := newTestCompiler(t)
	owner := &recordingGenerationMaterialization{}
	materializer := &resultMaterializer{
		digest: compiler.schemas.catalog.Digest(),
		owner:  owner,
		err:    errors.New("prepare failed with owner"),
	}
	factory, err := newGenerationFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	desired := mustGenerationSnapshot(t, 43, nil, nil)
	_, err = factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err == nil || owner.closeCalls != 1 || owner.closeCtx != nil {
		t.Fatalf("prepare error=%v closeCalls=%d closeCtx=%v", err, owner.closeCalls, owner.closeCtx)
	}
}

func TestGenerationFactoryRejectsNilAndInvalidOwner(t *testing.T) {
	compiler := newTestCompiler(t)
	desired := mustGenerationSnapshot(t, 44, nil, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	for _, tt := range []struct {
		name  string
		owner secret.GenerationMaterialization
	}{
		{name: "nil"},
		{name: "typed nil", owner: (*recordingGenerationMaterialization)(nil)},
		{name: "invalid secrets", owner: &recordingGenerationMaterialization{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			materializer := &resultMaterializer{digest: compiler.schemas.catalog.Digest(), owner: tt.owner}
			factory, err := newGenerationFactory(compiler, materializer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := factory.prepareGenerationSecrets(context.Background(), ticket, desired, nil); err == nil {
				t.Fatal("prepare invalid owner error = nil")
			}
		})
	}
}

func TestPreparedSecretGenerationCloseIsConcurrentAndIdempotent(t *testing.T) {
	factory, _, _ := newScopedGenerationFactory(t, &countingScopedBroker{})
	desired := mustGenerationSnapshot(t, 45, nil, nil)
	prepared, err := factory.prepareGenerationSecrets(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	errorsByCaller := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Go(func() {
			errorsByCaller <- prepared.Close(context.Background())
		})
	}
	wait.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerationFactoryRequiresMatchingDeclarationCatalog(t *testing.T) {
	compiler := newTestCompiler(t)
	materializer := &resultMaterializer{}
	if _, err := newGenerationFactory(compiler, materializer); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("newGenerationFactory() error = %v, want ErrInvalidInput", err)
	}
}

func newScopedGenerationFactory(
	t *testing.T,
	broker *countingScopedBroker,
) (*generationFactory, *countingMaterializer, *[]string) {
	t.Helper()
	return newScopedGenerationFactoryWithCompiler(t, newTestCompiler(t), broker)
}

func newScopedGenerationFactoryWithCompiler(
	t *testing.T,
	compiler *Compiler,
	broker testutil.SecretResolver,
) (*generationFactory, *countingMaterializer, *[]string) {
	t.Helper()
	trace := &[]string{}
	materializer := &countingMaterializer{
		delegate: testutil.NewSecretMaterializer(broker, compiler.schemas.catalog),
		trace:    trace,
	}
	factory, err := newGenerationFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer, trace
}
