package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

const currentJournalSchemaVersion uint64 = 1

var (
	journalMetaBucket         = []byte("generation_meta")
	desiredHeadBucket         = []byte("generation_desired_head")
	desiredRevisionBucket     = []byte("generation_desired_revisions")
	artifactBucket            = []byte("generation_artifacts")
	publishedHeadBucket       = []byte("generation_published_heads")
	publicationTxnBucket      = []byte("generation_publication_transactions")
	providerCursorBucket      = []byte("generation_provider_cursors")
	publicationDecisionBucket = []byte("generation_publication_decisions")

	schemaVersionKey       = []byte("schema_version")
	integrityAlgorithmKey  = []byte("integrity_algorithm")
	desiredHeadRevisionKey = []byte("revision")
	desiredHeadArtifactKey = []byte("artifact")

	journalBuckets = [][]byte{
		journalMetaBucket,
		desiredHeadBucket,
		desiredRevisionBucket,
		artifactBucket,
		publishedHeadBucket,
		publicationTxnBucket,
		providerCursorBucket,
		publicationDecisionBucket,
	}
)

type JournalOptions struct {
	LegacyResourceBuckets []string
}

type artifactEnvelope struct {
	Digest  [32]byte `json:"digest"`
	Size    uint64   `json:"size"`
	Payload []byte   `json:"payload"`
}

type snapshotWire struct {
	Revision   uint64                 `json:"revision"`
	Resources  []generation.Resource  `json:"resources"`
	Tombstones []generation.Tombstone `json:"tombstones"`
}

func OpenJournal(path string, options JournalOptions) (*Store, error) {
	if err := validateLegacyBucketNames(options.LegacyResourceBuckets); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open generation journal %q: %w", path, err)
	}
	storage := &Store{db: db, stopProducers: make(chan struct{})}
	if err := storage.initializeJournal(options.LegacyResourceBuckets); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return storage, nil
}

func (s *Store) initializeJournal(legacyBucketNames []string) error {
	initialized, err := inspectJournal(s.db)
	if err != nil {
		return err
	}
	if initialized {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := requireNoJournalBuckets(tx); err != nil {
			return err
		}

		legacyNames := uniqueBucketNames(legacyBucketNames)
		resources := make([]generation.Resource, 0)
		legacyExists := false
		for _, name := range legacyNames {
			bucket := tx.Bucket([]byte(name))
			if bucket == nil {
				continue
			}
			legacyExists = true
			if err := bucket.ForEach(func(id, value []byte) error {
				if value == nil {
					return generation.ErrIntegrity
				}
				resources = append(resources, generation.Resource{
					Key:   generation.ResourceKey{Kind: name, ID: string(bytes.Clone(id))},
					Value: bytes.Clone(value),
				})
				return nil
			}); err != nil {
				return err
			}
		}
		if !legacyExists {
			nonempty := false
			if err := tx.ForEach(func(_ []byte, _ *bolt.Bucket) error {
				nonempty = true
				return nil
			}); err != nil {
				return err
			}
			if nonempty {
				return generation.ErrIntegrity
			}
		}

		for _, name := range journalBuckets {
			if _, err := tx.CreateBucket(name); err != nil {
				return fmt.Errorf("create journal bucket %q: %w", name, err)
			}
		}
		meta := tx.Bucket(journalMetaBucket)
		if err := meta.Put(schemaVersionKey, encodeUint64(currentJournalSchemaVersion)); err != nil {
			return err
		}
		if err := meta.Put(integrityAlgorithmKey, []byte("sha256")); err != nil {
			return err
		}

		if legacyExists {
			snapshot, err := generation.NewSnapshot(1, resources, nil)
			if err != nil {
				return fmt.Errorf("build imported desired snapshot: %w", err)
			}
			if err := writeDesiredHeadTx(tx, snapshot); err != nil {
				return err
			}
		}
		for _, name := range legacyNames {
			if tx.Bucket([]byte(name)) == nil {
				continue
			}
			if err := tx.DeleteBucket([]byte(name)); err != nil {
				return fmt.Errorf("delete imported legacy bucket %q: %w", name, err)
			}
		}
		return nil
	})
}

func inspectJournal(db *bolt.DB) (bool, error) {
	initialized := false
	err := db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(journalMetaBucket)
		if meta == nil {
			if hasJournalDataBucket(tx) {
				return generation.ErrIntegrity
			}
			return nil
		}

		version, err := decodeUint64(meta.Get(schemaVersionKey))
		if err != nil || version == 0 {
			return generation.ErrIntegrity
		}
		if version > currentJournalSchemaVersion {
			return generation.ErrNewerSchema
		}
		if version != currentJournalSchemaVersion ||
			!bytes.Equal(meta.Get(integrityAlgorithmKey), []byte("sha256")) {
			return generation.ErrIntegrity
		}
		for _, name := range journalBuckets {
			if tx.Bucket(name) == nil {
				return generation.ErrIntegrity
			}
		}
		if err := validateDesiredHeadTx(tx); err != nil {
			return err
		}
		initialized = true
		return nil
	})
	return initialized, err
}

func requireNoJournalBuckets(tx *bolt.Tx) error {
	for _, name := range journalBuckets {
		if tx.Bucket(name) != nil {
			return generation.ErrIntegrity
		}
	}
	return nil
}

func hasJournalDataBucket(tx *bolt.Tx) bool {
	for _, name := range journalBuckets[1:] {
		if tx.Bucket(name) != nil {
			return true
		}
	}
	return false
}

func validateDesiredHeadTx(tx *bolt.Tx) error {
	head := tx.Bucket(desiredHeadBucket)
	revisions := tx.Bucket(desiredRevisionBucket)
	revisionBytes := head.Get(desiredHeadRevisionKey)
	artifactID := head.Get(desiredHeadArtifactKey)
	if revisionBytes == nil && artifactID == nil {
		if head.Stats().KeyN != 0 || revisions.Stats().KeyN != 0 || tx.Bucket(artifactBucket).Stats().KeyN != 0 {
			return generation.ErrIntegrity
		}
		return nil
	}
	if revisionBytes == nil || artifactID == nil || head.Stats().KeyN != 2 {
		return generation.ErrIntegrity
	}
	revision, err := decodeUint64(revisionBytes)
	if err != nil || revision == 0 {
		return generation.ErrIntegrity
	}
	if indexed := revisions.Get(encodeUint64(revision)); !bytes.Equal(indexed, artifactID) {
		return generation.ErrIntegrity
	}
	var lastRevision uint64
	if err := revisions.ForEach(func(encodedRevision, indexedArtifact []byte) error {
		indexedRevision, err := decodeUint64(encodedRevision)
		if err != nil || indexedRevision != lastRevision+1 || indexedRevision > revision || len(indexedArtifact) == 0 {
			return generation.ErrIntegrity
		}
		if _, err := loadDesiredSnapshotTx(tx, indexedRevision); err != nil {
			return err
		}
		lastRevision = indexedRevision
		return nil
	}); err != nil {
		return err
	}
	if lastRevision != revision {
		return generation.ErrIntegrity
	}
	return nil
}

func writeDesiredHeadTx(tx *bolt.Tx, snapshot generation.Snapshot) error {
	payload, err := snapshot.CanonicalBytes()
	if err != nil {
		return err
	}
	envelope := artifactEnvelope{
		Digest:  snapshot.Digest(),
		Size:    uint64(len(payload)),
		Payload: payload,
	}
	encodedEnvelope, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	id := snapshot.SnapshotID()
	if err := tx.Bucket(artifactBucket).Put([]byte(id), encodedEnvelope); err != nil {
		return err
	}
	if err := tx.Bucket(desiredRevisionBucket).Put(encodeUint64(snapshot.Revision()), []byte(id)); err != nil {
		return err
	}
	head := tx.Bucket(desiredHeadBucket)
	if err := head.Put(desiredHeadRevisionKey, encodeUint64(snapshot.Revision())); err != nil {
		return err
	}
	return head.Put(desiredHeadArtifactKey, []byte(id))
}

func loadDesiredSnapshotTx(tx *bolt.Tx, revision uint64) (generation.Snapshot, error) {
	head := tx.Bucket(desiredHeadBucket)
	revisions := tx.Bucket(desiredRevisionBucket)
	if head == nil || revisions == nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	var artifactID []byte
	if revision == 0 {
		revisionBytes := head.Get(desiredHeadRevisionKey)
		artifactID = head.Get(desiredHeadArtifactKey)
		if revisionBytes == nil && artifactID == nil {
			return generation.Snapshot{}, generation.ErrNotFound
		}
		if revisionBytes == nil || artifactID == nil {
			return generation.Snapshot{}, generation.ErrIntegrity
		}
		var err error
		revision, err = decodeUint64(revisionBytes)
		if err != nil || revision == 0 {
			return generation.Snapshot{}, generation.ErrIntegrity
		}
		if indexed := revisions.Get(encodeUint64(revision)); !bytes.Equal(indexed, artifactID) {
			return generation.Snapshot{}, generation.ErrIntegrity
		}
	} else {
		artifactID = revisions.Get(encodeUint64(revision))
		if artifactID == nil {
			return generation.Snapshot{}, generation.ErrNotFound
		}
	}
	id := string(artifactID)
	if id == "" {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	artifacts := tx.Bucket(artifactBucket)
	if artifacts == nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	encodedEnvelope := artifacts.Get([]byte(id))
	if encodedEnvelope == nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	var envelope artifactEnvelope
	if err := json.Unmarshal(encodedEnvelope, &envelope); err != nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	if err := verifyArtifact(id, envelope); err != nil {
		return generation.Snapshot{}, err
	}

	var wire snapshotWire
	if err := json.Unmarshal(envelope.Payload, &wire); err != nil {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	snapshot, err := generation.NewSnapshot(wire.Revision, wire.Resources, wire.Tombstones)
	if err != nil || snapshot.Revision() != revision {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	canonical, err := snapshot.CanonicalBytes()
	if err != nil || !bytes.Equal(canonical, envelope.Payload) ||
		snapshot.Digest() != envelope.Digest || snapshot.SnapshotID() != id {
		return generation.Snapshot{}, generation.ErrIntegrity
	}
	return snapshot, nil
}

func verifyArtifact(id string, envelope artifactEnvelope) error {
	digest := sha256.Sum256(envelope.Payload)
	if uint64(len(envelope.Payload)) != envelope.Size || digest != envelope.Digest ||
		id != "sha256:"+hex.EncodeToString(digest[:]) {
		return generation.ErrIntegrity
	}
	return nil
}

func (s *Store) Revisions(ctx context.Context) (generation.RevisionSet, error) {
	if err := contextErr(ctx); err != nil {
		return generation.RevisionSet{}, err
	}
	var revisions generation.RevisionSet
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		var err error
		revisions, err = loadRevisionSetTx(tx)
		if err != nil {
			return err
		}
		return contextErr(ctx)
	})
	return revisions, err
}

func uniqueBucketNames(names []string) []string {
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result
}

func validateLegacyBucketNames(names []string) error {
	for _, name := range names {
		if name == "" {
			return generation.ErrIntegrity
		}
		for _, reserved := range journalBuckets {
			if name == string(reserved) {
				return generation.ErrIntegrity
			}
		}
	}
	return nil
}

func encodeUint64(value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return encoded[:]
}

func decodeUint64(encoded []byte) (uint64, error) {
	if len(encoded) != 8 {
		return 0, generation.ErrIntegrity
	}
	return binary.BigEndian.Uint64(encoded), nil
}
