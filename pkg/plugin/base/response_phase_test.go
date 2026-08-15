package base

import (
	"net/http"
	"testing"
)

func TestCloneResponseStateDeepCopiesRepresentation(t *testing.T) {
	original := ResponseState{
		Status:  201,
		Header:  http.Header{"X-Test": {"one"}},
		Trailer: http.Header{"Grpc-Status": {"0"}},
		Body:    []byte("body"),
	}
	clone := CloneResponseState(original)
	clone.Header.Set("X-Test", "two")
	clone.Trailer.Set("Grpc-Status", "7")
	clone.Body[0] = 'B'
	if original.Header.Get("X-Test") != "one" || original.Trailer.Get("Grpc-Status") != "0" ||
		string(original.Body) != "body" {
		t.Fatalf("clone mutated original: %#v", original)
	}
}

func TestExtractResponseTrailersPreservesDeclarationsAndPrefixedValues(t *testing.T) {
	header := http.Header{
		"Trailer":                     {"Grpc-Status, Grpc-Message"},
		"Grpc-Status":                 {"0"},
		http.TrailerPrefix + "X-Late": {"value"},
		"X-Response":                  {"kept"},
	}

	trailer := ExtractResponseTrailers(header)
	if got := trailer.Get("Grpc-Status"); got != "0" {
		t.Fatalf("Grpc-Status = %q, want 0", got)
	}
	if _, ok := trailer["Grpc-Message"]; !ok {
		t.Fatal("empty Grpc-Message declaration was lost")
	}
	if got := trailer.Get("X-Late"); got != "value" {
		t.Fatalf("X-Late = %q, want value", got)
	}
	if header.Get("Trailer") != "" || header.Get("Grpc-Status") != "" ||
		header.Get(http.TrailerPrefix+"X-Late") != "" {
		t.Fatalf("trailer fields remain in response header: %#v", header)
	}
	if got := header.Get("X-Response"); got != "kept" {
		t.Fatalf("response header = %q, want kept", got)
	}
}

func TestCacheHitHolderDeepCopiesAndConsumesExactlyOnce(t *testing.T) {
	holder := &CacheHitResponseHolder{}
	state := CachedResponseState{
		Status:  200,
		Header:  http.Header{"X-Test": {"one"}},
		Trailer: http.Header{"X-Trailer": {"one"}},
		Body:    []byte("body"),
	}
	holder.Publish(state)
	state.Header.Set("X-Test", "changed-after-publish")
	state.Trailer.Set("X-Trailer", "changed-after-publish")
	state.Body[0] = 'X'
	got, err := holder.Consume()
	if err != nil || got.Status != 200 || got.Header.Get("X-Test") != "one" ||
		got.Trailer.Get("X-Trailer") != "one" || string(got.Body) != "body" {
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
