package generation

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewSnapshotCanonicalizesResourcesAndTombstones(t *testing.T) {
	snapshot, err := NewSnapshot(7, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
	}, []Tombstone{
		{Key: ResourceKey{Kind: "services", ID: "gone-b"}, Revision: 7},
		{Key: ResourceKey{Kind: "services", ID: "gone-a"}, Revision: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := snapshot.Resources()
	if got := resources[0].Key.ID; got != "a" {
		t.Fatalf("first resource = %q, want a", got)
	}
	tombstones := snapshot.Tombstones()
	if got := tombstones[0].Key.ID; got != "gone-a" {
		t.Fatalf("first tombstone = %q, want gone-a", got)
	}
	if !snapshot.Deleted(ResourceKey{Kind: "services", ID: "gone-b"}) {
		t.Fatal("explicit tombstone is not visible")
	}
	if snapshot.Deleted(ResourceKey{Kind: "services", ID: "present"}) {
		t.Fatal("missing tombstone is reported as deleted")
	}
}

func TestNewSnapshotRejectsInvalidAndDuplicateKeys(t *testing.T) {
	tests := []struct {
		name       string
		resources  []Resource
		tombstones []Tombstone
		want       string
	}{
		{
			name:      "resource kind",
			resources: []Resource{{Key: ResourceKey{ID: "r1"}}},
			want:      "resource key requires kind and id",
		},
		{
			name: "duplicate resource",
			resources: []Resource{
				{Key: ResourceKey{Kind: "routes", ID: "r1"}},
				{Key: ResourceKey{Kind: "routes", ID: "r1"}},
			},
			want: "duplicate resource routes/r1",
		},
		{
			name:       "tombstone revision",
			tombstones: []Tombstone{{Key: ResourceKey{Kind: "routes", ID: "r1"}}},
			want:       "invalid tombstone",
		},
		{
			name:      "resource tombstone overlap",
			resources: []Resource{{Key: ResourceKey{Kind: "routes", ID: "r1"}}},
			tombstones: []Tombstone{
				{Key: ResourceKey{Kind: "routes", ID: "r1"}, Revision: 2},
			},
			want: "resource and tombstone overlap at routes/r1",
		},
		{
			name: "duplicate tombstone",
			tombstones: []Tombstone{
				{Key: ResourceKey{Kind: "routes", ID: "r1"}, Revision: 1},
				{Key: ResourceKey{Kind: "routes", ID: "r1"}, Revision: 2},
			},
			want: "resource and tombstone overlap at routes/r1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewSnapshot(2, test.resources, test.tombstones)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewSnapshot() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewSnapshotRejectsInvalidUTF8Keys(t *testing.T) {
	invalid := string([]byte{0xff})
	for _, test := range []struct {
		name       string
		resources  []Resource
		tombstones []Tombstone
	}{
		{
			name:      "resource kind",
			resources: []Resource{{Key: ResourceKey{Kind: invalid, ID: "r1"}}},
		},
		{
			name:      "resource id",
			resources: []Resource{{Key: ResourceKey{Kind: "routes", ID: invalid}}},
		},
		{
			name: "tombstone kind",
			tombstones: []Tombstone{{
				Key: ResourceKey{Kind: invalid, ID: "r1"}, Revision: 1,
			}},
		},
		{
			name: "tombstone id",
			tombstones: []Tombstone{{
				Key: ResourceKey{Kind: "routes", ID: invalid}, Revision: 1,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewSnapshot(1, test.resources, test.tombstones)
			if err == nil || !strings.Contains(err.Error(), "valid UTF-8") {
				t.Fatalf("NewSnapshot() error = %v, want invalid UTF-8 rejection", err)
			}
		})
	}
}

func TestNewSnapshotAllowsSameIDAcrossKinds(t *testing.T) {
	snapshot, err := NewSnapshot(2, []Resource{
		{Key: ResourceKey{Kind: "services", ID: "same"}, Value: []byte("service")},
		{Key: ResourceKey{Kind: "routes", ID: "same"}, Value: []byte("route")},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resources := snapshot.Resources()
	if resources[0].Key.Kind != "routes" || resources[1].Key.Kind != "services" {
		t.Fatalf("resource order = %+v, want routes/same then services/same", resources)
	}
	for key, want := range map[ResourceKey]string{
		{Kind: "routes", ID: "same"}:   "route",
		{Kind: "services", ID: "same"}: "service",
	} {
		value, ok := snapshot.Lookup(key)
		if !ok || string(value) != want {
			t.Fatalf("Lookup(%+v) = %q, %t, want %q, true", key, value, ok, want)
		}
	}
}

func TestSnapshotDigestIsIndependentOfInputOrder(t *testing.T) {
	left, err := NewSnapshot(4, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
	}, []Tombstone{
		{Key: ResourceKey{Kind: "services", ID: "gone-b"}, Revision: 4},
		{Key: ResourceKey{Kind: "services", ID: "gone-a"}, Revision: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSnapshot(4, []Resource{
		{Key: ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"a"}`)},
		{Key: ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"b"}`)},
	}, []Tombstone{
		{Key: ResourceKey{Kind: "services", ID: "gone-a"}, Revision: 3},
		{Key: ResourceKey{Kind: "services", ID: "gone-b"}, Revision: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest() != right.Digest() || left.SnapshotID() != right.SnapshotID() {
		t.Fatalf("same snapshot has different identity: %x/%x", left.Digest(), right.Digest())
	}
	if !strings.HasPrefix(left.SnapshotID(), "sha256:") || len(left.SnapshotID()) != len("sha256:")+64 {
		t.Fatalf("SnapshotID() = %q, want lowercase SHA-256 identifier", left.SnapshotID())
	}
	if encoded, err := left.CanonicalBytes(); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(encoded, []byte("digest")) {
		t.Fatalf("CanonicalBytes() encoded digest: %s", encoded)
	}
}

func TestSnapshotDigestGoldenCanonicalJSON(t *testing.T) {
	snapshot, err := NewSnapshot(11, []Resource{{
		Key:   ResourceKey{Kind: "routes", ID: "r1"},
		Value: []byte(`{"id":"r1"}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical := `{"revision":11,"resources":[{"key":{"kind":"routes","id":"r1"},"value":"eyJpZCI6InIxIn0="}],"tombstones":null}`
	if string(canonical) != wantCanonical {
		t.Fatalf("CanonicalBytes() = %s, want %s", canonical, wantCanonical)
	}
	const wantID = "sha256:650371cc69db57babec14b6c21c1cf848aa39b91ddf3205cf1f3e6d4bd87036e"
	if snapshot.SnapshotID() != wantID {
		t.Fatalf("SnapshotID() = %q, want %q", snapshot.SnapshotID(), wantID)
	}

	empty, err := NewSnapshot(11, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyCanonical, err := empty.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	wantEmptyCanonical := `{"revision":11,"resources":null,"tombstones":null}`
	if string(emptyCanonical) != wantEmptyCanonical {
		t.Fatalf("empty CanonicalBytes() = %s, want %s", emptyCanonical, wantEmptyCanonical)
	}
	const wantEmptyID = "sha256:e1162a9c77080712f6f559bc4be69cf7e199db1a72cec5f8222e74704bee3a70"
	if empty.SnapshotID() != wantEmptyID {
		t.Fatalf("empty SnapshotID() = %q, want %q", empty.SnapshotID(), wantEmptyID)
	}
}

func TestSnapshotAccessorsPreserveStructuralImmutability(t *testing.T) {
	inputValue := []byte(`{"id":"r1"}`)
	inputResources := []Resource{{
		Key:   ResourceKey{Kind: "routes", ID: "r1"},
		Value: inputValue,
	}}
	inputTombstones := []Tombstone{{
		Key: ResourceKey{Kind: "services", ID: "gone"}, Revision: 12,
	}}
	snapshot, err := NewSnapshot(12, inputResources, inputTombstones)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical, err := snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := snapshot.Digest()
	wantID := snapshot.SnapshotID()

	inputValue[0] = 'x'
	inputResources[0].Key.ID = "changed-input"
	inputTombstones[0].Key.ID = "changed-input"

	resources := snapshot.Resources()
	resources[0].Key.ID = "changed-accessor"
	resources[0].Value[0] = 'x'
	tombstones := snapshot.Tombstones()
	tombstones[0].Key.ID = "changed-accessor"

	lookup, ok := snapshot.Lookup(ResourceKey{Kind: "routes", ID: "r1"})
	if !ok {
		t.Fatal("Lookup() did not find resource before mutation")
	}
	lookup[0] = 'x'

	cloneResources := snapshot.Clone().Resources()
	cloneResources[0].Key.ID = "changed-clone-accessor"
	cloneResources[0].Value[0] = 'x'

	gotCanonical, err := snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("canonical bytes changed through public API: got %s, want %s", gotCanonical, wantCanonical)
	}
	if snapshot.Revision() != 12 || snapshot.Digest() != wantDigest || snapshot.SnapshotID() != wantID {
		t.Fatalf(
			"identity changed through public API: revision=%d digest=%x id=%q",
			snapshot.Revision(),
			snapshot.Digest(),
			snapshot.SnapshotID(),
		)
	}
	value, ok := snapshot.Lookup(ResourceKey{Kind: "routes", ID: "r1"})
	if !ok || string(value) != `{"id":"r1"}` {
		t.Fatalf("Lookup() after mutation = %q, %t", value, ok)
	}
	if !snapshot.Deleted(ResourceKey{Kind: "services", ID: "gone"}) {
		t.Fatal("tombstone changed through public API")
	}
}

func TestSnapshotCloneDoesNotShareMutableStorage(t *testing.T) {
	originalValue := []byte(`{"id":"a"}`)
	snapshot, err := NewSnapshot(5, []Resource{{
		Key:   ResourceKey{Kind: "routes", ID: "a"},
		Value: originalValue,
	}}, []Tombstone{{Key: ResourceKey{Kind: "services", ID: "gone"}, Revision: 5}})
	if err != nil {
		t.Fatal(err)
	}
	originalValue[0] = 'x'
	if snapshot.Resources()[0].Value[0] == 'x' {
		t.Fatal("NewSnapshot shared caller resource bytes")
	}

	clone := snapshot.Clone()
	clone.resources[0].Value[0] = 'x'
	clone.resources[0].Key.ID = "changed"
	clone.tombstones[0].Key.ID = "changed"
	if bytes.Equal(clone.resources[0].Value, snapshot.resources[0].Value) {
		t.Fatal("Clone shared resource bytes")
	}
	if snapshot.resources[0].Key.ID != "a" || snapshot.tombstones[0].Key.ID != "gone" {
		t.Fatal("Clone shared resource or tombstone slices")
	}
	if clone.Digest() != snapshot.Digest() {
		t.Fatal("Clone changed immutable digest")
	}
}

func TestSnapshotClonePreservesEmptyCanonicalIdentity(t *testing.T) {
	snapshot, err := NewSnapshot(9, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := snapshot.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := snapshot.Clone().CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cloned, original) {
		t.Fatalf("Clone() canonical bytes = %s, want %s", cloned, original)
	}
}

func TestSnapshotLookupReturnsClonedValue(t *testing.T) {
	snapshot, err := NewSnapshot(3, []Resource{{
		Key:   ResourceKey{Kind: "routes", ID: "a"},
		Value: []byte(`{"id":"a"}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := snapshot.Lookup(ResourceKey{Kind: "routes", ID: "a"})
	if !ok {
		t.Fatal("Lookup() did not find resource")
	}
	value[0] = 'x'
	again, ok := snapshot.Lookup(ResourceKey{Kind: "routes", ID: "a"})
	if !ok || again[0] == 'x' {
		t.Fatal("Lookup() exposed mutable internal storage")
	}
	if _, ok := snapshot.Lookup(ResourceKey{Kind: "routes", ID: "missing"}); ok {
		t.Fatal("Lookup() found missing resource")
	}
}
