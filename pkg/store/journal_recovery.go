package store

import (
	"context"
	"errors"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	bolt "go.etcd.io/bbolt"
)

const recoveryArtifactIntegrityCode = "artifact-integrity"

// Recover is a startup-only operation. The coordinator must call it before
// starting providers or any other writer because it owns the journal's single
// recovery transaction and discards all transactions left pending by a prior
// process.
func (s *Store) Recover(ctx context.Context) (generation.RecoveryState, error) {
	if err := contextErr(ctx); err != nil {
		return generation.RecoveryState{}, err
	}
	var state generation.RecoveryState
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		initialized, err := verifyJournalMetaTx(tx)
		if err != nil {
			return err
		}
		if !initialized {
			return generation.ErrIntegrity
		}
		if err := validateDesiredHeadTx(tx); err != nil {
			return err
		}

		desired, err := loadDesiredSnapshotTx(tx, 0)
		if err != nil && !errors.Is(err, generation.ErrNotFound) {
			return err
		}
		if err == nil {
			state.Desired = desired
			state.Revisions.Desired = desired.Revision()
		}
		state.Published = make(map[generation.Domain]generation.PublishedGeneration)

		heads := tx.Bucket(publishedHeadBucket)
		present := make(map[generation.Domain]bool, 2)
		if err := heads.ForEach(func(key, value []byte) error {
			domain := generation.Domain(key)
			if !validPublicationDomain(domain) {
				return generation.ErrIntegrity
			}
			present[domain] = true
			return nil
		}); err != nil {
			return err
		}
		for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
			if !present[domain] {
				continue
			}
			published, err := loadPublishedTx(tx, domain)
			if err != nil || published.Artifact.Revision > state.Revisions.Desired {
				state.Failures = append(state.Failures, generation.RecoveryFailure{
					Domain: domain,
					Code:   recoveryArtifactIntegrityCode,
				})
				continue
			}
			state.Published[domain] = published
			switch domain {
			case generation.DomainHTTP:
				state.Revisions.HTTP = published.Artifact.Revision
			case generation.DomainStream:
				state.Revisions.Stream = published.Artifact.Revision
			}
		}

		pending := tx.Bucket(publicationTxnBucket)
		type pendingEntry struct {
			key    []byte
			bucket bool
		}
		entries := make([]pendingEntry, 0, pending.Stats().KeyN)
		if err := pending.ForEach(func(key, value []byte) error {
			entries = append(entries, pendingEntry{key: slices.Clone(key), bucket: value == nil})
			return nil
		}); err != nil {
			return err
		}
		for _, entry := range entries {
			var err error
			if entry.bucket {
				err = pending.DeleteBucket(entry.key)
			} else {
				err = pending.Delete(entry.key)
			}
			if err != nil {
				return err
			}
		}
		return contextErr(ctx)
	})
	if err != nil {
		return generation.RecoveryState{}, err
	}
	return cloneRecoveryState(state), nil
}

func cloneRecoveryState(state generation.RecoveryState) generation.RecoveryState {
	cloned := state
	cloned.Desired = state.Desired.Clone()
	cloned.Published = make(
		map[generation.Domain]generation.PublishedGeneration,
		len(state.Published),
	)
	for domain, published := range state.Published {
		cloned.Published[domain] = clonePublishedGeneration(published)
	}
	cloned.Failures = slices.Clone(state.Failures)
	return cloned
}
