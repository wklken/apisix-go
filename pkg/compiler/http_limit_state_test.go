package compiler

import (
	"context"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestHTTPRateLimitStateOutlivesCreatingGenerationWhileReused(t *testing.T) {
	sharedResources := runtime.NewResourceRegistry()
	first, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	second, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	first.registry, second.registry = sharedResources, sharedResources

	firstState, err := first.acquireHTTPRateLimitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	secondState, err := second.acquireHTTPRateLimitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstState == nil || firstState != secondState || sharedResources.Len() != 1 {
		t.Fatalf("shared rate-limit state = %p/%p registry=%d", firstState, secondState, sharedResources.Len())
	}
	firstResult := firstState.FixedWindow("route", 2, 1, time.Minute, true)
	secondResult := secondState.FixedWindow("route", 2, 1, time.Minute, true)
	if firstResult.Remaining != 1 || secondResult.Remaining != 0 {
		t.Fatalf("shared quota results = %#v, %#v", firstResult, secondResult)
	}

	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sharedResources.Len() != 1 {
		t.Fatalf("registry after first retirement = %d, want 1", sharedResources.Len())
	}
	if result := secondState.FixedWindow("route", 2, 1, time.Minute, true); result.Allowed {
		t.Fatalf("quota reset with successor generation: %#v", result)
	}

	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sharedResources.Len() != 0 {
		t.Fatalf("registry after final retirement = %d, want 0", sharedResources.Len())
	}
	if result := secondState.FixedWindow("new", 2, 1, time.Minute, true); result.Allowed {
		t.Fatalf("closed shared state accepted quota mutation: %#v", result)
	}
}

func TestHTTPRateLimitStateRollbackReleasesTentativeLease(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	checkpoint, err := prepared.cleanup.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	state, err := prepared.acquireHTTPRateLimitState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.cleanup.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if fixture.registry.Len() != 0 {
		t.Fatalf("registry after rollback = %d, want 0", fixture.registry.Len())
	}
	if result := state.FixedWindow("route", 2, 1, time.Minute, true); result.Allowed {
		t.Fatalf("rolled-back state accepted quota mutation: %#v", result)
	}
}
