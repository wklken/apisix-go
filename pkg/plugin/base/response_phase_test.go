package base

import (
	"net/http"
	"testing"
)

func TestCloneResponseStateDeepCopiesRepresentation(t *testing.T) {
	original := ResponseState{Status: 201, Header: http.Header{"X-Test": {"one"}}, Body: []byte("body")}
	clone := CloneResponseState(original)
	clone.Header.Set("X-Test", "two")
	clone.Body[0] = 'B'
	if original.Header.Get("X-Test") != "one" || string(original.Body) != "body" {
		t.Fatalf("clone mutated original: %#v", original)
	}
}

func TestCacheHitHolderDeepCopiesAndConsumesExactlyOnce(t *testing.T) {
	holder := &CacheHitResponseHolder{}
	state := CachedResponseState{Status: 200, Header: http.Header{"X-Test": {"one"}}, Body: []byte("body")}
	holder.Publish(state)
	state.Header.Set("X-Test", "changed-after-publish")
	state.Body[0] = 'X'
	got, err := holder.Consume()
	if err != nil || got.Status != 200 || got.Header.Get("X-Test") != "one" || string(got.Body) != "body" {
		t.Fatalf("Consume() = %#v, err=%v", got, err)
	}
	got.Header.Set("X-Test", "two")
	got.Body[0] = 'B'
	if _, err := holder.Consume(); err != ErrCacheHitResponseAlreadyConsumed {
		t.Fatalf("second Consume() err=%v, want %v", err, ErrCacheHitResponseAlreadyConsumed)
	}

	missing := &CacheHitResponseHolder{}
	if _, published, err := missing.ConsumePublished(); err != nil || published {
		t.Fatalf("missing ConsumePublished() = published:%v err:%v", published, err)
	}
	if _, _, err := missing.ConsumePublished(); err != ErrCacheHitResponseAlreadyConsumed {
		t.Fatalf("second missing ConsumePublished() err=%v", err)
	}
}
