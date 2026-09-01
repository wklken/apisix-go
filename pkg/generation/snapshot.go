package generation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
)

func NewSnapshot(revision uint64, resources []Resource, tombstones []Tombstone) (Snapshot, error) {
	return NewSnapshotWithSource(revision, resources, tombstones, nil)
}

func NewSnapshotWithSource(
	revision uint64,
	resources []Resource,
	tombstones []Tombstone,
	collectionVersions map[string]string,
) (Snapshot, error) {
	result := Snapshot{revision: revision, collectionVersions: cloneStringMap(collectionVersions)}
	for kind, version := range result.collectionVersions {
		if kind == "" || version == "" || !utf8.ValidString(kind) || !utf8.ValidString(version) {
			return Snapshot{}, fmt.Errorf("invalid collection version")
		}
	}
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
		if err := validateResourceOrigin(resource.Origin); err != nil {
			return Snapshot{}, err
		}
		result.resources = append(result.resources, Resource{
			Key: resource.Key, Origin: resource.Origin, Value: bytes.Clone(resource.Value),
		})
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

func (s Snapshot) CollectionVersion(kind string) (string, bool) {
	version, ok := s.collectionVersions[kind]
	return version, ok
}

func (s Snapshot) CollectionVersions() map[string]string {
	return cloneStringMap(s.collectionVersions)
}

func (s Snapshot) Digest() [32]byte {
	return s.digest
}

func (s Snapshot) Clone() Snapshot {
	clone := s
	clone.resources = cloneResources(s.resources)
	clone.tombstones = slices.Clone(s.tombstones)
	clone.collectionVersions = cloneStringMap(s.collectionVersions)
	return clone
}

func (s Snapshot) Lookup(key ResourceKey) ([]byte, bool) {
	index, found := slices.BinarySearchFunc(s.resources, Resource{Key: key}, compareResource)
	if !found {
		return nil, false
	}
	return bytes.Clone(s.resources[index].Value), true
}

func (s Snapshot) LookupResource(key ResourceKey) (Resource, bool) {
	index, found := slices.BinarySearchFunc(s.resources, Resource{Key: key}, compareResource)
	if !found {
		return Resource{}, false
	}
	resource := s.resources[index]
	resource.Value = bytes.Clone(resource.Value)
	return resource, true
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
	type resourceWire struct {
		Key    ResourceKey     `json:"key"`
		Origin *ResourceOrigin `json:"origin,omitempty"`
		Value  []byte          `json:"value"`
	}
	var wireResources []resourceWire
	if resources != nil {
		wireResources = make([]resourceWire, 0, len(resources))
	}
	for _, resource := range resources {
		wire := resourceWire{Key: resource.Key, Value: resource.Value}
		if resource.Origin != (ResourceOrigin{}) {
			origin := resource.Origin
			wire.Origin = &origin
		}
		wireResources = append(wireResources, wire)
	}
	return json.Marshal(struct {
		Revision           uint64            `json:"revision"`
		Resources          []resourceWire    `json:"resources"`
		Tombstones         []Tombstone       `json:"tombstones"`
		CollectionVersions map[string]string `json:"collection_versions,omitempty"`
	}{
		Revision: s.revision, Resources: wireResources, Tombstones: tombstones,
		CollectionVersions: cloneStringMap(s.collectionVersions),
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

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	maps.Copy(clone, values)
	return clone
}

func validateResourceOrigin(origin ResourceOrigin) error {
	if origin == (ResourceOrigin{}) {
		return nil
	}
	if origin.Provider == "" || origin.ResourceKey == "" || origin.ModifiedIndex == "" ||
		!utf8.ValidString(origin.Provider) || !utf8.ValidString(origin.ResourceKey) ||
		!utf8.ValidString(origin.ModifiedIndex) {
		return fmt.Errorf("invalid resource origin")
	}
	return nil
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
