package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
)

func NewSnapshot(revision uint64, resources []Resource, tombstones []Tombstone) (Snapshot, error) {
	result := Snapshot{revision: revision}
	seen := make(map[ResourceKey]struct{}, len(resources)+len(tombstones))
	for _, resource := range resources {
		if resource.Key.Kind == "" || resource.Key.ID == "" {
			return Snapshot{}, fmt.Errorf("resource key requires kind and id")
		}
		if !utf8.ValidString(resource.Key.Kind) || !utf8.ValidString(resource.Key.ID) {
			return Snapshot{}, fmt.Errorf("resource key requires valid UTF-8 kind and id")
		}
		if _, exists := seen[resource.Key]; exists {
			return Snapshot{}, fmt.Errorf("duplicate resource %s/%s", resource.Key.Kind, resource.Key.ID)
		}
		seen[resource.Key] = struct{}{}
		result.resources = append(result.resources, Resource{Key: resource.Key, Value: bytes.Clone(resource.Value)})
	}
	for _, tombstone := range tombstones {
		if tombstone.Key.Kind == "" || tombstone.Key.ID == "" || tombstone.Revision == 0 {
			return Snapshot{}, fmt.Errorf("invalid tombstone")
		}
		if !utf8.ValidString(tombstone.Key.Kind) || !utf8.ValidString(tombstone.Key.ID) {
			return Snapshot{}, fmt.Errorf("tombstone key requires valid UTF-8 kind and id")
		}
		if _, exists := seen[tombstone.Key]; exists {
			return Snapshot{}, fmt.Errorf(
				"resource and tombstone overlap at %s/%s",
				tombstone.Key.Kind,
				tombstone.Key.ID,
			)
		}
		seen[tombstone.Key] = struct{}{}
		result.tombstones = append(result.tombstones, tombstone)
	}
	slices.SortFunc(result.resources, compareResource)
	slices.SortFunc(result.tombstones, compareTombstone)
	encoded, err := result.CanonicalBytes()
	if err != nil {
		return Snapshot{}, err
	}
	result.digest = sha256.Sum256(encoded)
	return result, nil
}

func (s Snapshot) Revision() uint64 {
	return s.revision
}

func (s Snapshot) Resources() []Resource {
	return cloneResources(s.resources)
}

func (s Snapshot) Tombstones() []Tombstone {
	return slices.Clone(s.tombstones)
}

func (s Snapshot) Digest() [32]byte {
	return s.digest
}

func (s Snapshot) Clone() Snapshot {
	clone := s
	clone.resources = cloneResources(s.resources)
	clone.tombstones = slices.Clone(s.tombstones)
	return clone
}

func (s Snapshot) Lookup(key ResourceKey) ([]byte, bool) {
	index, found := slices.BinarySearchFunc(s.resources, Resource{Key: key}, compareResource)
	if !found {
		return nil, false
	}
	return bytes.Clone(s.resources[index].Value), true
}

func (s Snapshot) Deleted(key ResourceKey) bool {
	_, found := slices.BinarySearchFunc(s.tombstones, Tombstone{Key: key}, compareTombstone)
	return found
}

func (s Snapshot) CanonicalBytes() ([]byte, error) {
	resources := cloneResources(s.resources)
	tombstones := slices.Clone(s.tombstones)
	slices.SortFunc(resources, compareResource)
	slices.SortFunc(tombstones, compareTombstone)
	return json.Marshal(struct {
		Revision   uint64      `json:"revision"`
		Resources  []Resource  `json:"resources"`
		Tombstones []Tombstone `json:"tombstones"`
	}{
		Revision:   s.revision,
		Resources:  resources,
		Tombstones: tombstones,
	})
}

func (s Snapshot) SnapshotID() string {
	return "sha256:" + hex.EncodeToString(s.digest[:])
}

func cloneResources(resources []Resource) []Resource {
	clone := slices.Clone(resources)
	for index := range clone {
		clone[index].Value = bytes.Clone(clone[index].Value)
	}
	return clone
}

func compareResource(left, right Resource) int {
	if byKind := strings.Compare(left.Key.Kind, right.Key.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.Key.ID, right.Key.ID)
}

func compareTombstone(left, right Tombstone) int {
	if byKind := strings.Compare(left.Key.Kind, right.Key.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.Key.ID, right.Key.ID)
}
