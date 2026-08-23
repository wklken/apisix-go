package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

func TestJournalApplyDesiredPersistsPutDeleteAndHistoryAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	put := desiredBatch("etcd", "41", generation.DomainHTTP, generation.Mutation{
		Type:  generation.MutationPut,
		Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
		Value: []byte(`{"id":"r1"}`),
	})
	first, err := journal.ApplyDesired(context.Background(), put)
	if err != nil {
		t.Fatal(err)
	}
	deleteBatch := desiredBatch("etcd", "42", generation.DomainHTTP, generation.Mutation{
		Type: generation.MutationDelete,
		Key:  generation.ResourceKey{Kind: "routes", ID: "r1"},
	})
	second, err := journal.ApplyDesired(context.Background(), deleteBatch)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := journal.ApplyDesired(context.Background(), desiredBatch(
		"etcd", "43", generation.DomainHTTP, deleteBatch.Mutations[0],
	))
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredRevision != 1 || second.DesiredRevision != 2 || refreshed.DesiredRevision != 3 {
		t.Fatalf(
			"revisions = %d/%d/%d, want 1/2/3",
			first.DesiredRevision,
			second.DesiredRevision,
			refreshed.DesiredRevision,
		)
	}
	assertDesiredValue(t, journal, 1, put.Mutations[0].Key, put.Mutations[0].Value)
	assertDesiredTombstone(t, journal, 2, put.Mutations[0].Key, 2)
	assertDesiredTombstone(t, journal, 3, put.Mutations[0].Key, 3)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertDesiredValue(t, reopened, 1, put.Mutations[0].Key, put.Mutations[0].Value)
	assertDesiredTombstone(t, reopened, 0, put.Mutations[0].Key, 3)
	if _, err := reopened.LoadDesired(context.Background(), 4); !errors.Is(err, generation.ErrNotFound) {
		t.Fatalf("LoadDesired(4) error = %v, want ErrNotFound", err)
	}
}

func TestJournalReplacementTombstonesMissingAndEmptyAuthoritativeState(t *testing.T) {
	journal := openTestJournal(t)
	_, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "1"},
		Mutations: []generation.Mutation{
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "routes", ID: "old-route"},
				Value: []byte("route"),
			},
			{
				Type:  generation.MutationPut,
				Key:   generation.ResourceKey{Kind: "services", ID: "old-service"},
				Value: []byte("service"),
			},
		},
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	})
	if err != nil {
		t.Fatal(err)
	}
	replacementKey := generation.ResourceKey{Kind: "routes", ID: "new-route"}
	second, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor:         generation.ProviderCursor{Provider: "etcd", Revision: "2"},
		ReplaceManaged: true,
		Mutations: []generation.Mutation{{
			Type: generation.MutationPut, Key: replacementKey, Value: []byte("new"),
		}},
		RequiredDomains: []generation.Domain{
			generation.DomainStream, generation.DomainHTTP, generation.DomainHTTP,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !domainsEqual(second.RequiredDomains, generation.DomainHTTP, generation.DomainStream) {
		t.Fatalf("replacement domains = %v", second.RequiredDomains)
	}
	assertDesiredTombstone(t, journal, 2, generation.ResourceKey{Kind: "routes", ID: "old-route"}, 2)
	assertDesiredTombstone(t, journal, 2, generation.ResourceKey{Kind: "services", ID: "old-service"}, 2)
	assertDesiredValue(t, journal, 2, replacementKey, []byte("new"))

	third, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor:          generation.ProviderCursor{Provider: "etcd", Revision: "3"},
		ReplaceManaged:  true,
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := journal.LoadDesired(context.Background(), third.DesiredRevision)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Resources()) != 0 {
		t.Fatalf("empty replacement retained resources: %+v", snapshot.Resources())
	}
	assertTombstoneRevision(t, snapshot, replacementKey, 3)
	assertTombstoneRevision(t, snapshot, generation.ResourceKey{Kind: "routes", ID: "old-route"}, 2)
}

func TestJournalApplyDesiredPreservesOrderedSameKeyLastWriteWins(t *testing.T) {
	key := generation.ResourceKey{Kind: "routes", ID: "same"}
	t.Run("put then delete", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket, err := journal.ApplyDesired(context.Background(), desiredBatch(
			"etcd", "1", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("value")},
			generation.Mutation{Type: generation.MutationDelete, Key: key},
		))
		if err != nil {
			t.Fatal(err)
		}
		assertDesiredTombstone(t, journal, ticket.DesiredRevision, key, 1)
	})
	t.Run("delete then put", func(t *testing.T) {
		journal := openTestJournal(t)
		ticket, err := journal.ApplyDesired(context.Background(), desiredBatch(
			"etcd", "1", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationDelete, Key: key},
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("value")},
		))
		if err != nil {
			t.Fatal(err)
		}
		assertDesiredValue(t, journal, ticket.DesiredRevision, key, []byte("value"))
	})
}

func TestJournalCursorCurrentReplayIsIdempotentAcrossRestartAndIsolated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	value := []byte("original")
	batch := desiredBatch("etcd", "41", generation.DomainHTTP, generation.Mutation{
		Type: generation.MutationPut, Key: key, Value: value,
	})
	first, err := journal.ApplyDesired(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	value[0] = 'x'
	batch.Mutations[0].Value[1] = 'x'
	first.RequiredDomains[0] = generation.DomainStream
	pristine := desiredBatch("etcd", "41", generation.DomainHTTP, generation.Mutation{
		Type: generation.MutationPut, Key: key, Value: []byte("original"),
	})
	replay, err := journal.ApplyDesired(context.Background(), pristine)
	if err != nil {
		t.Fatal(err)
	}
	if replay.DesiredRevision != 1 || !domainsEqual(replay.RequiredDomains, generation.DomainHTTP) {
		t.Fatalf("replay = %+v", replay)
	}
	replay.RequiredDomains[0] = generation.DomainStream
	assertDesiredValue(t, journal, 1, key, []byte("original"))
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restartedReplay, err := reopened.ApplyDesired(context.Background(), pristine)
	if err != nil {
		t.Fatal(err)
	}
	if restartedReplay.DesiredRevision != 1 || !domainsEqual(restartedReplay.RequiredDomains, generation.DomainHTTP) {
		t.Fatalf("restart replay = %+v", restartedReplay)
	}
	assertRevisions(t, reopened, generation.RevisionSet{Desired: 1})

	loaded, err := reopened.LoadDesired(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	resources := loaded.Resources()
	resources[0].Value[0] = 'x'
	lookup, _ := loaded.Lookup(key)
	lookup[0] = 'x'
	assertDesiredValue(t, reopened, 1, key, []byte("original"))
}

func TestJournalCursorStaleReplayAndConflictMatrix(t *testing.T) {
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	base := desiredBatch("etcd", "same", generation.DomainHTTP,
		generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("one")},
		generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("two")},
	)
	conflicts := []struct {
		name  string
		batch generation.DesiredBatch
	}{
		{name: "value", batch: desiredBatch("etcd", "same", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("changed")},
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("two")})},
		{name: "order", batch: desiredBatch("etcd", "same", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("two")},
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("one")})},
		{name: "type", batch: desiredBatch("etcd", "same", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationDelete, Key: key},
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("two")})},
		{name: "domain", batch: desiredBatch("etcd", "same", generation.DomainStream,
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("one")},
			generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("two")})},
		{name: "replace", batch: generation.DesiredBatch{
			Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "same"}, ReplaceManaged: true,
			Mutations:       base.Mutations,
			RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
		}},
	}
	for _, test := range conflicts {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			if _, err := journal.ApplyDesired(context.Background(), base); err != nil {
				t.Fatal(err)
			}
			_, err := journal.ApplyDesired(context.Background(), test.batch)
			if !errors.Is(err, generation.ErrCursorConflict) {
				t.Fatalf("ApplyDesired() error = %v, want ErrCursorConflict", err)
			}
			assertRevisions(t, journal, generation.RevisionSet{Desired: 1})
		})
	}

	journal := openTestJournal(t)
	if _, err := journal.ApplyDesired(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(context.Background(), desiredBatch(
		"etcd", "new", generation.DomainHTTP,
		generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte("new")},
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(context.Background(), base); !errors.Is(err, generation.ErrStaleCursor) {
		t.Fatalf("stale ApplyDesired() error = %v, want ErrStaleCursor", err)
	}
}

func TestJournalCursorStaleReplayAfterRestartDoesNotAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	old := desiredBatch("etcd", "old", generation.DomainHTTP)
	if _, err := journal.ApplyDesired(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(
		context.Background(),
		desiredBatch("etcd", "new", generation.DomainHTTP),
	); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.ApplyDesired(context.Background(), old); !errors.Is(err, generation.ErrStaleCursor) {
		t.Fatalf("stale restart ApplyDesired() error = %v, want ErrStaleCursor", err)
	}
	assertRevisions(t, reopened, generation.RevisionSet{Desired: 2})
}

func TestJournalCursorProviderSwitchAndCollisionResistantIdentity(t *testing.T) {
	journal := openTestJournal(t)
	first := generation.DesiredBatch{
		Cursor:          generation.ProviderCursor{Provider: "a/b", Revision: "c"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	if _, err := journal.ApplyDesired(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor:          generation.ProviderCursor{Provider: "a", Revision: "b/c"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}); !errors.Is(err, generation.ErrProviderConflict) {
		t.Fatalf("incremental provider switch error = %v, want ErrProviderConflict", err)
	}
	second := generation.DesiredBatch{
		Cursor:          generation.ProviderCursor{Provider: "a", Revision: "b/c"},
		ReplaceManaged:  true,
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	}
	if _, err := journal.ApplyDesired(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(cursorRecordKey(first.Cursor), cursorRecordKey(second.Cursor)) {
		t.Fatal("cursor hash key collided for ambiguous delimiter pairs")
	}
	if _, err := journal.ApplyDesired(context.Background(), first); !errors.Is(err, generation.ErrProviderConflict) {
		t.Fatalf("inactive provider replay error = %v, want ErrProviderConflict", err)
	}
}

func TestJournalApplyDesiredValidatesEmptyDomainCursorMutationAndValues(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	tests := []struct {
		name  string
		batch generation.DesiredBatch
	}{
		{name: "empty provider", batch: desiredBatch("", "1", generation.DomainHTTP)},
		{name: "empty revision", batch: desiredBatch("etcd", "", generation.DomainHTTP)},
		{name: "invalid provider utf8", batch: desiredBatch(invalidUTF8, "1", generation.DomainHTTP)},
		{name: "invalid revision utf8", batch: desiredBatch("etcd", invalidUTF8, generation.DomainHTTP)},
		{name: "invalid key utf8", batch: desiredBatch(
			"etcd",
			"1",
			generation.DomainHTTP,
			generation.Mutation{
				Type: generation.MutationPut,
				Key:  generation.ResourceKey{Kind: invalidUTF8, ID: "r1"},
			},
		)},
		{name: "invalid key id utf8", batch: desiredBatch(
			"etcd",
			"1",
			generation.DomainHTTP,
			generation.Mutation{
				Type: generation.MutationPut,
				Key:  generation.ResourceKey{Kind: "routes", ID: invalidUTF8},
			},
		)},
		{name: "empty key", batch: desiredBatch("etcd", "1", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationPut, Key: generation.ResourceKey{Kind: "routes"}})},
		{name: "unknown mutation", batch: desiredBatch("etcd", "1", generation.DomainHTTP,
			generation.Mutation{Type: "change", Key: key})},
		{name: "unknown domain", batch: desiredBatch("etcd", "1", "udp")},
		{name: "delete value", batch: desiredBatch("etcd", "1", generation.DomainHTTP,
			generation.Mutation{Type: generation.MutationDelete, Key: key, Value: []byte("unexpected")})},
		{name: "mutation without domain", batch: desiredBatch("etcd", "1", "",
			generation.Mutation{Type: generation.MutationPut, Key: key})},
		{name: "replacement missing stream", batch: generation.DesiredBatch{
			Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "1"}, ReplaceManaged: true,
			RequiredDomains: []generation.Domain{generation.DomainHTTP},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			if _, err := journal.ApplyDesired(context.Background(), test.batch); err == nil {
				t.Fatal("ApplyDesired() error = nil, want validation failure")
			}
			assertRevisions(t, journal, generation.RevisionSet{})
		})
	}

	journal := openTestJournal(t)
	for index, batch := range []generation.DesiredBatch{
		{Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "empty"}},
		{
			Cursor:          generation.ProviderCursor{Provider: "etcd", Revision: "forced"},
			RequiredDomains: []generation.Domain{generation.DomainHTTP},
		},
		{
			Cursor:          generation.ProviderCursor{Provider: "etcd", Revision: "replacement"},
			ReplaceManaged:  true,
			RequiredDomains: []generation.Domain{generation.DomainStream, generation.DomainHTTP},
		},
	} {
		ticket, err := journal.ApplyDesired(context.Background(), batch)
		if err != nil {
			t.Fatalf("empty batch %d: %v", index, err)
		}
		if ticket.DesiredRevision != uint64(index+1) {
			t.Fatalf("empty batch %d revision = %d", index, ticket.DesiredRevision)
		}
	}
}

func TestJournalCursorNormalizesDomainsForReplay(t *testing.T) {
	journal := openTestJournal(t)
	first := generation.DesiredBatch{
		Cursor: generation.ProviderCursor{Provider: "etcd", Revision: "same"},
		RequiredDomains: []generation.Domain{
			generation.DomainStream, generation.DomainHTTP, generation.DomainStream,
		},
	}
	ticket, err := journal.ApplyDesired(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := journal.ApplyDesired(context.Background(), generation.DesiredBatch{
		Cursor:          first.Cursor,
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replay.DesiredRevision != ticket.DesiredRevision ||
		!domainsEqual(replay.RequiredDomains, generation.DomainHTTP, generation.DomainStream) {
		t.Fatalf("normalized replay = %+v, original = %+v", replay, ticket)
	}
}

func TestJournalCursorBatchDigestGoldenDistinguishesNilAndEmptyPut(t *testing.T) {
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	tests := []struct {
		name  string
		value []byte
		want  string
	}{
		{
			name: "nil",
			want: "a5a1ac946bae09404eb31e22b7cc7f9026759ed629e13f63f6d63f79a8fcf49e",
		},
		{
			name:  "empty",
			value: []byte{},
			want:  "32b0e97db71178f05a45d642ae774dfbe630e2ad24398dc5bc4e7b94956b23f4",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			digest, err := digestDesiredBatch(desiredBatch(
				"etcd",
				"same",
				generation.DomainHTTP,
				generation.Mutation{Type: generation.MutationPut, Key: key, Value: test.value},
			))
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(digest[:]); got != test.want {
				t.Fatalf("digest = %s, want %s", got, test.want)
			}
		})
	}
}

func TestJournalDesiredBatchWireGolden(t *testing.T) {
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	wire := desiredBatchWire{
		Cursor:          providerCursorWire{Provider: "etcd", Revision: "same"},
		Mutations:       []desiredMutationWire{{Type: generation.MutationPut, Key: key}},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(
		`{"cursor":{"provider":"etcd","revision":"same"},"replace_managed":false,` +
			`"mutations":[{"type":"put","key":{"kind":"routes","id":"r1"},"value":null}],` +
			`"required_domains":["http"]}`,
	)
	if !bytes.Equal(encoded, want) {
		t.Fatalf("desired batch wire = %s, want %s", encoded, want)
	}
	digest, err := digestDesiredBatch(desiredBatch(
		"etcd",
		"same",
		generation.DomainHTTP,
		generation.Mutation{Type: generation.MutationPut, Key: key},
	))
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256.Sum256(want) {
		t.Fatalf("desired batch digest does not match wire golden")
	}
}

func TestJournalCursorRecordWirePayloadGolden(t *testing.T) {
	cursor := generation.ProviderCursor{Provider: "etcd", Revision: "same"}
	encoded, err := encodeCursorRecord(cursorRecord{
		Cursor: cursor,
		Ticket: generation.ApplyTicket{
			DesiredRevision: 1,
			Cursor:          cursor,
			RequiredDomains: []generation.Domain{generation.DomainHTTP},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var envelope cursorRecordEnvelope
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	zeroDigest := "[" + strings.TrimSuffix(strings.Repeat("0,", sha256.Size), ",") + "]"
	want := fmt.Appendf(nil,
		`{"cursor":{"provider":"etcd","revision":"same"},"batch_digest":%s,`+
			`"ticket":{"desired_revision":1,"desired_digest":%s,`+
			`"cursor":{"provider":"etcd","revision":"same"},"required_domains":["http"]}}`,
		zeroDigest,
		zeroDigest,
	)
	if !bytes.Equal(envelope.Payload, want) {
		t.Fatalf("cursor record payload = %s, want %s", envelope.Payload, want)
	}
	if envelope.Digest != sha256.Sum256(want) {
		t.Fatalf("cursor record envelope digest does not match payload golden")
	}
}

func TestJournalCursorDistinguishesNilAndEmptyPutAndDetectsMissingOrTamperedCurrentRecord(t *testing.T) {
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	nilBatch := desiredBatch("etcd", "same", generation.DomainHTTP,
		generation.Mutation{Type: generation.MutationPut, Key: key, Value: nil})
	emptyBatch := desiredBatch("etcd", "same", generation.DomainHTTP,
		generation.Mutation{Type: generation.MutationPut, Key: key, Value: []byte{}})
	journal := openTestJournal(t)
	if _, err := journal.ApplyDesired(context.Background(), nilBatch); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(context.Background(), emptyBatch); !errors.Is(err, generation.ErrCursorConflict) {
		t.Fatalf("nil/empty replay error = %v, want ErrCursorConflict", err)
	}

	for _, test := range []struct {
		name   string
		tamper func(*bolt.Bucket, []byte) error
	}{
		{name: "missing", tamper: func(bucket *bolt.Bucket, key []byte) error { return bucket.Delete(key) }},
		{name: "tampered", tamper: func(bucket *bolt.Bucket, key []byte) error {
			return bucket.Put(key, []byte(`{"digest":"tampered"}`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			candidate, err := OpenJournal(path, JournalOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := candidate.ApplyDesired(context.Background(), nilBatch); err != nil {
				t.Fatal(err)
			}
			if err := candidate.db.Update(func(tx *bolt.Tx) error {
				return test.tamper(tx.Bucket(providerCursorBucket), cursorRecordKey(nilBatch.Cursor))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := candidate.ApplyDesired(
				context.Background(),
				nilBatch,
			); !errors.Is(
				err,
				generation.ErrIntegrity,
			) {
				t.Fatalf("tampered replay error = %v, want ErrIntegrity", err)
			}
			_ = candidate.Close()
		})
	}
}

func TestJournalCursorDetectsMissingOrTamperedActiveAuthorityRecord(t *testing.T) {
	batch := desiredBatch("etcd", "same", generation.DomainHTTP)
	for _, test := range []struct {
		name   string
		tamper func(*bolt.Bucket) error
	}{
		{name: "missing", tamper: func(bucket *bolt.Bucket) error { return bucket.Delete(activeProviderRecordKey) }},
		{name: "tampered", tamper: func(bucket *bolt.Bucket) error {
			return bucket.Put(activeProviderRecordKey, []byte(`{"digest":"tampered"}`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			if _, err := journal.ApplyDesired(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			if err := journal.db.Update(func(tx *bolt.Tx) error {
				return test.tamper(tx.Bucket(providerCursorBucket))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.ApplyDesired(context.Background(), batch); !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("authority replay error = %v, want ErrIntegrity", err)
			}
		})
	}
}

func TestJournalCursorRejectsCoherentActiveAuthorityRollback(t *testing.T) {
	journal := openTestJournal(t)
	first := desiredBatch("etcd", "first", generation.DomainHTTP)
	second := desiredBatch("etcd", "second", generation.DomainHTTP)
	if _, err := journal.ApplyDesired(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	var oldRecord []byte
	if err := journal.db.View(func(tx *bolt.Tx) error {
		oldRecord = bytes.Clone(tx.Bucket(providerCursorBucket).Get(cursorRecordKey(first.Cursor)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(providerCursorBucket).Put(activeProviderRecordKey, oldRecord)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(
		context.Background(),
		desiredBatch("etcd", "third", generation.DomainHTTP),
	); !errors.Is(err, generation.ErrIntegrity) {
		t.Fatalf("ApplyDesired() error = %v, want ErrIntegrity", err)
	}
	assertRevisions(t, journal, generation.RevisionSet{Desired: 2})
}

func TestJournalApplyDesiredAndLoadDesiredAfterCloseReturnErrors(t *testing.T) {
	journal := openTestJournal(t)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.ApplyDesired(
		context.Background(),
		desiredBatch("etcd", "1", generation.DomainHTTP),
	); err == nil {
		t.Fatal("ApplyDesired() after Close error = nil")
	}
	if _, err := journal.LoadDesired(context.Background(), 0); err == nil {
		t.Fatal("LoadDesired() after Close error = nil")
	}
}

func TestJournalApplyDesiredCanceledAndOverflowLeaveNoWrites(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		journal := openTestJournal(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := journal.ApplyDesired(ctx, desiredBatch("etcd", "1", generation.DomainHTTP))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ApplyDesired() error = %v, want context canceled", err)
		}
		assertRevisions(t, journal, generation.RevisionSet{})
	})

	t.Run("overflow", func(t *testing.T) {
		journal := openTestJournal(t)
		maximum, err := generation.NewSnapshot(math.MaxUint64, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.db.Update(func(tx *bolt.Tx) error { return writeDesiredHeadTx(tx, maximum) }); err != nil {
			t.Fatal(err)
		}
		beforeArtifacts := bucketKeyCount(t, journal.db, artifactBucket)
		_, err = journal.ApplyDesired(context.Background(), desiredBatch("etcd", "1", generation.DomainHTTP))
		if err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("ApplyDesired() error = %v, want overflow", err)
		}
		if got := bucketKeyCount(t, journal.db, artifactBucket); got != beforeArtifacts {
			t.Fatalf("artifact count = %d, want %d", got, beforeArtifacts)
		}
		assertRevisions(t, journal, generation.RevisionSet{Desired: math.MaxUint64})
	})
}

func desiredBatch(
	provider string,
	revision string,
	domain generation.Domain,
	mutations ...generation.Mutation,
) generation.DesiredBatch {
	batch := generation.DesiredBatch{
		Cursor:    generation.ProviderCursor{Provider: provider, Revision: revision},
		Mutations: mutations,
	}
	if domain != "" {
		batch.RequiredDomains = []generation.Domain{domain}
	}
	return batch
}

func assertDesiredValue(
	t *testing.T,
	journal *Store,
	revision uint64,
	key generation.ResourceKey,
	want []byte,
) {
	t.Helper()
	snapshot, err := journal.LoadDesired(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := snapshot.Lookup(key)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("Lookup(%+v) = %q, %t, want %q", key, got, ok, want)
	}
	if snapshot.Deleted(key) {
		t.Fatalf("resource %+v is also tombstoned", key)
	}
}

func assertDesiredTombstone(
	t *testing.T,
	journal *Store,
	revision uint64,
	key generation.ResourceKey,
	wantRevision uint64,
) {
	t.Helper()
	snapshot, err := journal.LoadDesired(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}
	assertTombstoneRevision(t, snapshot, key, wantRevision)
}

func assertTombstoneRevision(
	t *testing.T,
	snapshot generation.Snapshot,
	key generation.ResourceKey,
	wantRevision uint64,
) {
	t.Helper()
	if !snapshot.Deleted(key) {
		t.Fatalf("resource %+v is not tombstoned", key)
	}
	for _, tombstone := range snapshot.Tombstones() {
		if tombstone.Key == key {
			if tombstone.Revision != wantRevision {
				t.Fatalf("tombstone %+v revision = %d, want %d", key, tombstone.Revision, wantRevision)
			}
			return
		}
	}
	t.Fatalf("tombstone %+v is absent", key)
}

func domainsEqual(got []generation.Domain, want ...generation.Domain) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func bucketKeyCount(t *testing.T, db *bolt.DB, name []byte) int {
	t.Helper()
	count := 0
	if err := db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket(name).Stats().KeyN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
