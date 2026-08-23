package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

func TestOpenJournalInitializesEmptyAtRevisionZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := journal.Revisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revisions != (generation.RevisionSet{}) {
		t.Fatalf("revisions = %+v, want zero", revisions)
	}
	assertJournalBuckets(t, journal.db)
	if err := journal.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(artifactBucket).Stats().KeyN != 0 {
			t.Fatal("empty journal wrote an artifact")
		}
		if tx.Bucket(desiredHeadBucket).Stats().KeyN != 0 {
			t.Fatal("empty journal wrote a desired head")
		}
		if tx.Bucket(publishedHeadBucket).Stats().KeyN != 0 {
			t.Fatal("empty journal wrote a published head")
		}
		if _, err := loadDesiredSnapshotTx(tx, 0); !errors.Is(err, generation.ErrNotFound) {
			t.Fatalf("loadDesiredSnapshotTx(empty) error = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o, want 600", info.Mode().Perm())
	}
}

func TestOpenJournalImportsLegacyBucketsAsDesiredOnly(t *testing.T) {
	raw := []byte{0xff, 0x00, 0x01}
	path := seedLegacyDatabase(t, map[string]map[string][]byte{
		"routes": {"r1": raw},
		"audit":  {"keep": []byte("unlisted")},
	})
	journal, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})

	snapshot := loadDesiredSnapshot(t, journal, 1)
	if snapshot.Revision() != 1 {
		t.Fatalf("desired revision = %d, want 1", snapshot.Revision())
	}
	value, ok := snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "r1"})
	if !ok || !bytes.Equal(value, raw) {
		t.Fatalf("legacy value = %v, %t, want raw %v", value, ok, raw)
	}
	current := loadDesiredSnapshot(t, journal, 0)
	if current.SnapshotID() != snapshot.SnapshotID() {
		t.Fatalf("current snapshot = %q, want %q", current.SnapshotID(), snapshot.SnapshotID())
	}
	assertBucketMissing(t, journal.db, "routes")
	assertBucketPresent(t, journal.db, "audit")
	if err := journal.db.View(func(tx *bolt.Tx) error {
		wantRevision := []byte{0, 0, 0, 0, 0, 0, 0, 1}
		if got := tx.Bucket(desiredHeadBucket).Get(desiredHeadRevisionKey); !bytes.Equal(got, wantRevision) {
			t.Fatalf("desired head revision = %v, want big-endian %v", got, wantRevision)
		}
		headArtifact := tx.Bucket(desiredHeadBucket).Get(desiredHeadArtifactKey)
		if indexed := tx.Bucket(desiredRevisionBucket).Get(wantRevision); !bytes.Equal(indexed, headArtifact) {
			t.Fatalf("desired revision index = %q, want head artifact %q", indexed, headArtifact)
		}
		if tx.Bucket(publishedHeadBucket).Stats().KeyN != 0 {
			t.Fatal("legacy import invented a published head")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenJournalImportsExistingEmptyLegacyBucketAtRevisionOne(t *testing.T) {
	path := seedLegacyDatabase(t, map[string]map[string][]byte{"routes": {}})
	journal, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
	snapshot := loadDesiredSnapshot(t, journal, 1)
	if snapshot.Resources() != nil || snapshot.Tombstones() != nil {
		t.Fatalf("empty legacy snapshot = %+v/%+v, want nil slices", snapshot.Resources(), snapshot.Tombstones())
	}
	assertBucketMissing(t, journal.db, "routes")
}

func TestOpenJournalRejectsNonemptyDatabaseWithoutListedLegacyBuckets(t *testing.T) {
	path := seedLegacyDatabase(t, map[string]map[string][]byte{
		"routes": {"r1": []byte(`{"id":"r1"}`)},
	})
	before := readFileDigest(t, path)
	_, err := OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("unlisted legacy database was modified")
	}
	assertDatabaseLockReleased(t, path)
}

func TestOpenJournalRejectsInvalidLegacyBucketNamesWithoutMutation(t *testing.T) {
	for _, name := range []string{"", string(journalMetaBucket), string(artifactBucket)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "empty.db")
			withBoltUpdate(t, path, func(*bolt.Tx) error { return nil })
			before := readFileDigest(t, path)
			_, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{name}})
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
			}
			if after := readFileDigest(t, path); after != before {
				t.Fatal("invalid legacy bucket option modified database")
			}
			assertDatabaseLockReleased(t, path)
		})
	}
}

func TestOpenJournalRejectsInvalidLegacyBucketNamesBeforeCreatingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	_, err := OpenJournal(path, JournalOptions{
		LegacyResourceBuckets: []string{string(journalMetaBucket)},
	})
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid options created database: Stat() error = %v", err)
	}
}

func TestOpenJournalReopenIsIdempotent(t *testing.T) {
	path := seedLegacyDatabase(t, map[string]map[string][]byte{
		"routes": {"r1": []byte(`{"id":"r1"}`)},
	})
	first, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
	if err != nil {
		t.Fatal(err)
	}
	firstSnapshot := loadDesiredSnapshot(t, first, 1)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	assertRevisions(t, second, generation.RevisionSet{Desired: 1})
	secondSnapshot := loadDesiredSnapshot(t, second, 1)
	if firstSnapshot.SnapshotID() != secondSnapshot.SnapshotID() {
		t.Fatalf("reopen snapshot = %q, want %q", secondSnapshot.SnapshotID(), firstSnapshot.SnapshotID())
	}
	if err := second.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(artifactBucket).Stats().KeyN != 1 {
			t.Fatalf("artifact count = %d, want 1", tx.Bucket(artifactBucket).Stats().KeyN)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenJournalMigratesVersionOneTransactionally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(journalMetaBucket).Put(schemaVersionKey, encodeUint64(1))
	})

	migrated, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	if currentJournalSchemaVersion != 2 {
		t.Fatalf("current schema version = %d, want 2", currentJournalSchemaVersion)
	}
	assertJournalBuckets(t, migrated.db)
}

func TestOpenJournalVersionOneMigrationRollsBackOnCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		if err := tx.Bucket(journalMetaBucket).Put(schemaVersionKey, encodeUint64(1)); err != nil {
			return err
		}
		return tx.Bucket(desiredHeadBucket).Put([]byte("corrupt"), []byte("value"))
	})
	before := readFileDigest(t, path)

	_, err = OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("failed version-one migration modified database")
	}
	assertDatabaseLockReleased(t, path)
}

func TestVersionOneReaderRejectsVersionTwoBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "v2-committed", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	before := readFileDigest(t, path)

	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		_, _, err := verifyJournalMetaCompatibleTx(tx, 1)
		return err
	})
	if closeErr := db.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, generation.ErrNewerSchema) {
		t.Fatalf("version-one verification error = %v, want ErrNewerSchema", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("version-one reader modified version-two database")
	}
}

func TestDesiredRevisionIndexLoadsHistoricalSnapshots(t *testing.T) {
	journal := openTestJournal(t)
	first, err := generation.NewSnapshot(1, []generation.Resource{mappingResource("r1")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generation.NewSnapshot(2, []generation.Resource{mappingResource("r2")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		if err := writeDesiredHeadTx(tx, first); err != nil {
			return err
		}
		return writeDesiredHeadTx(tx, second)
	}); err != nil {
		t.Fatal(err)
	}
	if got := loadDesiredSnapshot(t, journal, 1); got.SnapshotID() != first.SnapshotID() {
		t.Fatalf("revision 1 = %q, want %q", got.SnapshotID(), first.SnapshotID())
	}
	if got := loadDesiredSnapshot(t, journal, 2); got.SnapshotID() != second.SnapshotID() {
		t.Fatalf("revision 2 = %q, want %q", got.SnapshotID(), second.SnapshotID())
	}
	if got := loadDesiredSnapshot(t, journal, 0); got.SnapshotID() != second.SnapshotID() {
		t.Fatalf("current revision = %q, want %q", got.SnapshotID(), second.SnapshotID())
	}
}

func mappingResource(id string) generation.Resource {
	return generation.Resource{
		Key: generation.ResourceKey{Kind: "routes", ID: id}, Value: []byte(`{"id":"` + id + `"}`),
	}
}

func TestOpenJournalRejectsUnknownNewerSchemaWithoutMutationAndReleasesLock(t *testing.T) {
	path := seedSchemaVersion(t, currentJournalSchemaVersion+1)
	before := readFileDigest(t, path)
	_, err := OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrNewerSchema) {
		t.Fatalf("OpenJournal() error = %v, want ErrNewerSchema", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("newer-schema database was modified")
	}
	assertDatabaseLockReleased(t, path)
}

func TestJournalSchemaRejectsZeroAndPartialStateWithoutMutation(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T) string
	}{
		{name: "explicit version zero", seed: func(t *testing.T) string { return seedSchemaVersion(t, 0) }},
		{name: "current version metadata only", seed: func(t *testing.T) string {
			return seedSchemaVersion(t, currentJournalSchemaVersion)
		}},
		{name: "journal data without metadata", seed: func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "partial.db")
			withBoltUpdate(t, path, func(tx *bolt.Tx) error {
				_, err := tx.CreateBucket(artifactBucket)
				return err
			})
			return path
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := test.seed(t)
			before := readFileDigest(t, path)
			_, err := OpenJournal(path, JournalOptions{})
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
			}
			if after := readFileDigest(t, path); after != before {
				t.Fatal("corrupt schema was modified")
			}
			assertDatabaseLockReleased(t, path)
		})
	}
}

func TestJournalSchemaRejectsOrphanArtifactWithoutDesiredHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(artifactBucket).Put([]byte("orphan"), []byte("artifact"))
	})
	before := readFileDigest(t, path)
	_, err = OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("orphan-artifact database was modified")
	}
	assertDatabaseLockReleased(t, path)
}

func TestJournalSchemaRejectsUnknownDesiredHeadKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		return tx.Bucket(desiredHeadBucket).Put([]byte("junk"), []byte("value"))
	})
	before := readFileDigest(t, path)
	_, err = OpenJournal(path, JournalOptions{})
	if !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
	}
	if after := readFileDigest(t, path); after != before {
		t.Fatal("unknown-head-key database was modified")
	}
	assertDatabaseLockReleased(t, path)
}

func TestJournalSchemaRejectsMalformedDesiredRevisionIndex(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*bolt.Bucket) error
	}{
		{name: "unknown key shape", tamper: func(bucket *bolt.Bucket) error {
			return bucket.Put([]byte("short"), []byte("artifact"))
		}},
		{name: "missing current mapping", tamper: func(bucket *bolt.Bucket) error {
			return bucket.Delete(encodeUint64(1))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := seedLegacyDatabase(t, map[string]map[string][]byte{
				"routes": {"r1": []byte(`{"id":"r1"}`)},
			})
			journal, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			withBoltUpdate(t, path, func(tx *bolt.Tx) error {
				return test.tamper(tx.Bucket(desiredRevisionBucket))
			})
			before := readFileDigest(t, path)
			_, err = OpenJournal(path, JournalOptions{})
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
			}
			if after := readFileDigest(t, path); after != before {
				t.Fatal("malformed-revision-index database was modified")
			}
			assertDatabaseLockReleased(t, path)
		})
	}
}

func TestJournalSchemaRejectsFutureAndMismatchedHistoricalRevisionIndex(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*bolt.Bucket, generation.Snapshot, generation.Snapshot) error
	}{
		{name: "future missing artifact", tamper: func(bucket *bolt.Bucket, _, _ generation.Snapshot) error {
			return bucket.Put(encodeUint64(3), []byte("missing"))
		}},
		{name: "historical revision mismatch", tamper: func(
			bucket *bolt.Bucket, _ generation.Snapshot, second generation.Snapshot,
		) error {
			return bucket.Put(encodeUint64(1), []byte(second.SnapshotID()))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, first, second := seedDesiredHistory(t)
			withBoltUpdate(t, path, func(tx *bolt.Tx) error {
				return test.tamper(tx.Bucket(desiredRevisionBucket), first, second)
			})
			before := readFileDigest(t, path)
			_, err := OpenJournal(path, JournalOptions{})
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
			}
			if after := readFileDigest(t, path); after != before {
				t.Fatal("corrupt historical index was modified")
			}
			assertDatabaseLockReleased(t, path)
		})
	}
}

func seedDesiredHistory(t *testing.T) (string, generation.Snapshot, generation.Snapshot) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := generation.NewSnapshot(1, []generation.Resource{mappingResource("r1")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generation.NewSnapshot(2, []generation.Resource{mappingResource("r2")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		if err := writeDesiredHeadTx(tx, first); err != nil {
			return err
		}
		return writeDesiredHeadTx(tx, second)
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	return path, first, second
}

func TestArtifactEnvelopeRejectsPayloadSizeDigestIDAndCanonicalTampering(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope)
	}{
		{name: "payload", tamper: func(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope) {
			t.Helper()
			envelope.Payload = append(envelope.Payload, ' ')
			putEnvelope(t, tx, id, envelope)
		}},
		{name: "size", tamper: func(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope) {
			t.Helper()
			envelope.Size++
			putEnvelope(t, tx, id, envelope)
		}},
		{name: "digest", tamper: func(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope) {
			t.Helper()
			envelope.Digest[0] ^= 0xff
			putEnvelope(t, tx, id, envelope)
		}},
		{name: "id", tamper: func(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope) {
			t.Helper()
			wrongID := "sha256:" + strings.Repeat("0", 64)
			if err := tx.Bucket(artifactBucket).Delete([]byte(id)); err != nil {
				t.Fatal(err)
			}
			putEnvelope(t, tx, wrongID, envelope)
			if err := tx.Bucket(desiredHeadBucket).Put(desiredHeadArtifactKey, []byte(wrongID)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "canonical payload", tamper: func(t *testing.T, tx *bolt.Tx, id string, _ artifactEnvelope) {
			t.Helper()
			payload := []byte(
				`{"tombstones":null,"resources":[` +
					`{"value":"eyJpZCI6InIxIn0=","key":{"id":"r1","kind":"routes"}}` +
					`],"revision":1}`,
			)
			digest := sha256.Sum256(payload)
			newID := "sha256:" + hex.EncodeToString(digest[:])
			if err := tx.Bucket(artifactBucket).Delete([]byte(id)); err != nil {
				t.Fatal(err)
			}
			putEnvelope(t, tx, newID, artifactEnvelope{Digest: digest, Size: uint64(len(payload)), Payload: payload})
			if err := tx.Bucket(desiredHeadBucket).Put(desiredHeadArtifactKey, []byte(newID)); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := seedLegacyDatabase(t, map[string]map[string][]byte{
				"routes": {"r1": []byte(`{"id":"r1"}`)},
			})
			journal, err := OpenJournal(path, JournalOptions{LegacyResourceBuckets: []string{"routes"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			withBoltUpdate(t, path, func(tx *bolt.Tx) error {
				id := string(tx.Bucket(desiredHeadBucket).Get(desiredHeadArtifactKey))
				var envelope artifactEnvelope
				if err := json.Unmarshal(tx.Bucket(artifactBucket).Get([]byte(id)), &envelope); err != nil {
					return err
				}
				test.tamper(t, tx, id, envelope)
				return nil
			})

			_, err = OpenJournal(path, JournalOptions{})
			if !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("OpenJournal() error = %v, want ErrIntegrity", err)
			}
			assertDatabaseLockReleased(t, path)
		})
	}
}

func seedLegacyDatabase(t *testing.T, contents map[string]map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		for name, rows := range contents {
			bucket, err := tx.CreateBucketIfNotExists([]byte(name))
			if err != nil {
				return err
			}
			for key, value := range rows {
				if err := bucket.Put([]byte(key), value); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return path
}

func seedSchemaVersion(t *testing.T, version uint64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "schema.db")
	withBoltUpdate(t, path, func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket(journalMetaBucket)
		if err != nil {
			return err
		}
		return bucket.Put(schemaVersionKey, encodeUint64(version))
	})
	return path
}

func withBoltUpdate(t *testing.T, path string, update func(*bolt.Tx) error) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(update); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func assertDatabaseLockReleased(t *testing.T, path string) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("database lock was not released: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertJournalBuckets(t *testing.T, db *bolt.DB) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		for _, name := range journalBuckets {
			if tx.Bucket(name) == nil {
				t.Fatalf("journal bucket %q is missing", name)
			}
		}
		meta := tx.Bucket(journalMetaBucket)
		if got := binary.BigEndian.Uint64(meta.Get(schemaVersionKey)); got != currentJournalSchemaVersion {
			t.Fatalf("schema version = %d, want %d", got, currentJournalSchemaVersion)
		}
		if got := string(meta.Get(integrityAlgorithmKey)); got != "sha256" {
			t.Fatalf("integrity algorithm = %q, want sha256", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRevisions(t *testing.T, journal *Store, want generation.RevisionSet) {
	t.Helper()
	got, err := journal.Revisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("revisions = %+v, want %+v", got, want)
	}
}

func openTestJournal(t *testing.T) *Store {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.db"), JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal
}

func loadDesiredSnapshot(t *testing.T, journal *Store, revision uint64) generation.Snapshot {
	t.Helper()
	var snapshot generation.Snapshot
	if err := journal.db.View(func(tx *bolt.Tx) error {
		var err error
		snapshot, err = loadDesiredSnapshotTx(tx, revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertBucketMissing(t *testing.T, db *bolt.DB, name string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(name)) != nil {
			t.Fatalf("bucket %q still exists", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertBucketPresent(t *testing.T, db *bolt.DB, name string) {
	t.Helper()
	if err := db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(name)) == nil {
			t.Fatalf("bucket %q is missing", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func putEnvelope(t *testing.T, tx *bolt.Tx, id string, envelope artifactEnvelope) {
	t.Helper()
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Bucket(artifactBucket).Put([]byte(id), encoded); err != nil {
		t.Fatal(err)
	}
}
