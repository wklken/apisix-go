package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

func TestJournalRecoverEmptyAndPublishedDomains(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		journal := openTestJournal(t)
		state, err := journal.Recover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if state.Revisions != (generation.RevisionSet{}) || len(state.Published) != 0 ||
			len(state.Failures) != 0 {
			t.Fatalf("Recover() = %+v, want empty state", state)
		}
	})

	for _, test := range []struct {
		name    string
		domains []generation.Domain
	}{
		{name: "http only", domains: []generation.Domain{generation.DomainHTTP}},
		{name: "stream only", domains: []generation.Domain{generation.DomainStream}},
		{name: "both domains", domains: []generation.Domain{generation.DomainHTTP, generation.DomainStream}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyDesiredForPublication(t, journal, "1", test.domains...)
			set := publicationSet(t, ticket, test.domains...)
			token, err := journal.Stage(context.Background(), ticket, set)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Commit(context.Background(), token); err != nil {
				t.Fatal(err)
			}
			state, err := journal.Recover(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if state.Revisions.Desired != ticket.DesiredRevision ||
				len(state.Published) != len(test.domains) ||
				len(state.Failures) != 0 {
				t.Fatalf("Recover() = %+v", state)
			}
			for _, domain := range test.domains {
				if state.Published[domain].Artifact != set.Domains[domain].Artifact ||
					revisionForDomain(state.Revisions, domain) != ticket.DesiredRevision {
					t.Fatalf("Recover(%s) = %+v", domain, state)
				}
			}
		})
	}
}

func TestJournalRecoverAcrossRestartAndDefensiveCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	journal, err := OpenJournal(path, JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	set := publicationSet(t, ticket, generation.DomainHTTP)
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
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
	state, err := reopened.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	published := state.Published[generation.DomainHTTP]
	resources := published.Snapshot.Resources()
	resources[0].Value[0] = 'x'
	state.Published[generation.DomainHTTP] = generation.PublishedGeneration{}
	state.Failures = append(
		state.Failures,
		generation.RecoveryFailure{Domain: "udp", Code: "mutated"},
	)

	again, err := reopened.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value, found := again.Published[generation.DomainHTTP].Snapshot.Lookup(resources[0].Key)
	if !found || value[0] == 'x' || len(again.Failures) != 0 || again.Revisions.HTTP != 1 {
		t.Fatalf("second Recover() reused caller-mutated state: %+v value=%q", again, value)
	}
}

func TestJournalRecoverIsolatesPublishedRevisionBeyondDesired(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(
		t,
		journal,
		"1",
		generation.DomainHTTP,
		generation.DomainStream,
	)
	set := publicationSet(t, ticket, generation.DomainHTTP, generation.DomainStream)
	token, err := journal.Stage(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	stream := set.Domains[generation.DomainStream]
	futureSnapshot := mustSnapshot(t, 2, stream.Snapshot.Resources(), stream.Snapshot.Tombstones())
	stream.Snapshot = futureSnapshot
	stream.Artifact = policyArtifact(generation.DomainStream, futureSnapshot)
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		if err := putArtifactTx(tx, stream.Snapshot); err != nil {
			return err
		}
		if err := putPublishedHeadTx(tx, generation.DomainStream, stream); err != nil {
			return err
		}
		return putDecisionsTx(tx, generation.DomainStream, 2, stream.Decisions)
	}); err != nil {
		t.Fatal(err)
	}
	state, err := journal.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Revisions != (generation.RevisionSet{Desired: 1, HTTP: 1}) ||
		len(state.Failures) != 1 || state.Failures[0].Domain != generation.DomainStream {
		t.Fatalf("Recover() = %+v, want future stream isolated", state)
	}
}

func TestJournalRecoverIsolatesKnownDomainPublicationCorruption(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, *Store, generation.Domain, generation.PublicationCandidate)
	}{
		{
			name: "head",
			corrupt: func(
				t *testing.T,
				journal *Store,
				domain generation.Domain,
				_ generation.PublicationCandidate,
			) {
				corruptBucketValue(t, journal, publishedHeadBucket, []byte(domain))
			},
		},
		{
			name: "nested head bucket",
			corrupt: func(
				t *testing.T,
				journal *Store,
				domain generation.Domain,
				_ generation.PublicationCandidate,
			) {
				if err := journal.db.Update(func(tx *bolt.Tx) error {
					heads := tx.Bucket(publishedHeadBucket)
					if err := heads.Delete([]byte(domain)); err != nil {
						return err
					}
					_, err := heads.CreateBucket([]byte(domain))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "artifact",
			corrupt: func(
				t *testing.T,
				journal *Store,
				_ generation.Domain,
				candidate generation.PublicationCandidate,
			) {
				corruptBucketValue(t, journal, artifactBucket, []byte(candidate.Artifact.Snapshot))
			},
		},
		{
			name: "decision",
			corrupt: func(
				t *testing.T,
				journal *Store,
				domain generation.Domain,
				candidate generation.PublicationCandidate,
			) {
				corruptBucketValue(
					t,
					journal,
					publicationDecisionBucket,
					publicationDecisionKey(domain, candidate.Artifact.Revision),
				)
			},
		},
		{
			name: "closure",
			corrupt: func(
				t *testing.T,
				journal *Store,
				domain generation.Domain,
				_ generation.PublicationCandidate,
			) {
				if err := journal.db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket(publishedHeadBucket)
					head, err := decodePublishedHead(
						bucket.Get([]byte(domain)),
						domain,
					)
					if err != nil {
						return err
					}
					wirePayload, err := json.Marshal(publishedHeadWire{
						Artifact:        artifactToWire(head.Artifact),
						Closure:         nil,
						DecisionsDigest: head.DecisionsDigest,
					})
					if err != nil {
						return err
					}
					encoded, err := encodePublicationEnvelope(wirePayload)
					if err != nil {
						return err
					}
					return bucket.Put([]byte(domain), encoded)
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		for _, corruptDomain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
			t.Run(test.name+"/"+string(corruptDomain), func(t *testing.T) {
				journal := openTestJournal(t)
				ticket := applyDesiredForPublication(
					t,
					journal,
					"1",
					generation.DomainHTTP,
					generation.DomainStream,
				)
				set := publicationSet(t, ticket, generation.DomainHTTP, generation.DomainStream)
				token, err := journal.Stage(context.Background(), ticket, set)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := journal.Commit(context.Background(), token); err != nil {
					t.Fatal(err)
				}
				test.corrupt(t, journal, corruptDomain, set.Domains[corruptDomain])

				state, err := journal.Recover(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				sibling := generation.DomainHTTP
				wantRevisions := generation.RevisionSet{Desired: 1, HTTP: 1}
				if corruptDomain == generation.DomainHTTP {
					sibling = generation.DomainStream
					wantRevisions = generation.RevisionSet{Desired: 1, Stream: 1}
				}
				if state.Revisions != wantRevisions || len(state.Published) != 1 ||
					state.Published[sibling].Artifact.Revision != 1 ||
					revisionForDomain(state.Revisions, corruptDomain) != 0 ||
					len(state.Failures) != 1 || state.Failures[0] != (generation.RecoveryFailure{
					Domain: corruptDomain, Code: "artifact-integrity",
				}) {
					t.Fatalf("Recover() = %+v, want isolated %s corruption", state, corruptDomain)
				}
			})
		}
	}
}

func TestJournalRecoverRejectsGlobalCorruptionAndPreservesPending(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, *Store)
		want    error
	}{
		{
			name: "desired",
			corrupt: func(t *testing.T, journal *Store) {
				corruptBucketValue(t, journal, desiredHeadBucket, desiredHeadArtifactKey)
			},
			want: generation.ErrIntegrity,
		},
		{
			name: "metadata",
			corrupt: func(t *testing.T, journal *Store) {
				putRecoveryValue(
					t,
					journal,
					journalMetaBucket,
					integrityAlgorithmKey,
					[]byte("md5"),
				)
			},
			want: generation.ErrIntegrity,
		},
		{
			name: "newer schema",
			corrupt: func(t *testing.T, journal *Store) {
				putRecoveryValue(
					t,
					journal,
					journalMetaBucket,
					schemaVersionKey,
					encodeUint64(currentJournalSchemaVersion+1),
				)
			},
			want: generation.ErrNewerSchema,
		},
		{
			name: "unknown published domain",
			corrupt: func(t *testing.T, journal *Store) {
				putRecoveryValue(t, journal, publishedHeadBucket, []byte("udp"), []byte("unknown"))
			},
			want: generation.ErrIntegrity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t)
			ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
			if _, err := journal.Stage(
				context.Background(), ticket, publicationSet(t, ticket, generation.DomainHTTP),
			); err != nil {
				t.Fatal(err)
			}
			test.corrupt(t, journal)
			state, err := journal.Recover(context.Background())
			if !errors.Is(err, test.want) || !zeroRecoveryState(state) {
				t.Fatalf("Recover() = %+v/%v, want zero/%v", state, err, test.want)
			}
			if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 1 {
				t.Fatalf("pending count = %d, want rollback-preserved 1", got)
			}
		})
	}
}

func TestJournalRecoverClearsPendingWithoutDecodingAndCancellationRollsBack(t *testing.T) {
	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	if _, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	); err != nil {
		t.Fatal(err)
	}
	putRecoveryValue(t, journal, publicationTxnBucket, []byte("corrupt"), []byte("not-json"))
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.Bucket(publicationTxnBucket).CreateBucket([]byte("nested-corrupt"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	canceled := newStepCancelContext(3)
	if state, err := journal.Recover(canceled); !errors.Is(err, context.Canceled) ||
		!zeroRecoveryState(state) {
		t.Fatalf("Recover(canceled) = %+v/%v", state, err)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 3 {
		t.Fatalf("pending count after rollback = %d, want 3", got)
	}

	state, err := journal.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Revisions.Desired != 1 || len(state.Published) != 0 {
		t.Fatalf("Recover() = %+v", state)
	}
	if got := bucketKeyCount(t, journal.db, publicationTxnBucket); got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
	second, err := journal.Recover(context.Background())
	if err != nil || second.Revisions != state.Revisions || len(second.Published) != 0 {
		t.Fatalf("second Recover() = %+v/%v", second, err)
	}
}

func corruptBucketValue(t *testing.T, journal *Store, bucketName, key []byte) {
	t.Helper()
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		value := bytes.Clone(bucket.Get(key))
		if len(value) == 0 {
			t.Fatalf("missing corruption target %q/%q", bucketName, key)
		}
		value[len(value)-1] ^= 1
		return bucket.Put(key, value)
	}); err != nil {
		t.Fatal(err)
	}
}

func putRecoveryValue(t *testing.T, journal *Store, bucketName, key, value []byte) {
	t.Helper()
	if err := journal.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put(key, value)
	}); err != nil {
		t.Fatal(err)
	}
}

func zeroRecoveryState(state generation.RecoveryState) bool {
	return state.Revisions == (generation.RevisionSet{}) && state.Desired.Revision() == 0 &&
		len(state.Published) == 0 && len(state.Failures) == 0
}
