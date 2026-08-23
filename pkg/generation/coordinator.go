package generation

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// PublicationEngine owns detached runtime candidates from Prepare until they
// are discarded, rolled back, or finalized after a durable journal commit.
type PublicationEngine interface {
	Prepare(
		context.Context,
		ApplyTicket,
		Snapshot,
		map[Domain]PublishedGeneration,
	) (PublicationSet, error)
	DiscardPrepared(context.Context, PublicationSet) error
	Activate(context.Context, PublicationToken, PublicationSet) error
	RollbackActivation(context.Context, PublicationToken, PublicationSet) error
	FinalizeActivation(context.Context, PublicationToken, PublicationSet)
	ConfirmActive(context.Context, PublicationSet) error
}

// Coordinator serializes the desired-to-runtime publication lifecycle. The
// process must use only one Coordinator for the journal's single writer.
type Coordinator struct {
	journal Journal
	engine  PublicationEngine
	mu      sync.Mutex
}

func NewCoordinator(journal Journal, engine PublicationEngine) *Coordinator {
	return &Coordinator{journal: journal, engine: engine}
}

func (c *Coordinator) Apply(ctx context.Context, batch DesiredBatch) (Acknowledgement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Acknowledgement{}, err
	}

	committed, err := c.journal.LoadAcknowledgement(ctx, batch.Cursor)
	if err == nil {
		set, loadErr := c.loadCommittedPublicationSet(ctx, batch.Cursor, committed)
		if loadErr != nil {
			return Acknowledgement{}, loadErr
		}
		if activeErr := c.engine.ConfirmActive(ctx, set); activeErr != nil {
			return Acknowledgement{}, activeErr
		}
		return cloneCoordinatorAck(committed), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Acknowledgement{}, err
	}

	ticket, err := c.journal.ApplyDesired(ctx, batch)
	if err != nil {
		return Acknowledgement{}, err
	}

	desired, err := c.journal.LoadDesired(ctx, ticket.DesiredRevision)
	if err != nil {
		return Acknowledgement{}, err
	}
	if desired.Revision() != ticket.DesiredRevision || desired.Digest() != ticket.DesiredDigest {
		return Acknowledgement{}, ErrIntegrity
	}
	previous := make(map[Domain]PublishedGeneration, len(ticket.RequiredDomains))
	for _, domain := range ticket.RequiredDomains {
		published, loadErr := c.journal.LoadPublished(ctx, domain)
		switch {
		case loadErr == nil:
			if published.Artifact.Domain != domain ||
				published.Artifact.Revision >= ticket.DesiredRevision {
				return Acknowledgement{}, ErrIntegrity
			}
			previous[domain] = published
		case errors.Is(loadErr, ErrNotFound):
			// A missing head is a valid first publication for this domain.
		default:
			return Acknowledgement{}, loadErr
		}
	}

	set, err := c.engine.Prepare(ctx, ticket, desired, previous)
	if err != nil {
		return Acknowledgement{}, err
	}
	token, err := c.journal.Stage(ctx, ticket, set)
	if err != nil {
		discardErr := c.engine.DiscardPrepared(context.WithoutCancel(ctx), set)
		return Acknowledgement{}, errors.Join(err, discardErr)
	}
	if err := c.engine.Activate(ctx, token, set); err != nil {
		cleanupCtx := context.WithoutCancel(ctx)
		rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
		abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("activation", err))
		return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
	}
	ack, err := c.journal.Commit(ctx, token)
	if err != nil {
		cleanupCtx := context.WithoutCancel(ctx)
		rollbackErr := c.engine.RollbackActivation(cleanupCtx, token, set)
		abortErr := c.journal.Abort(cleanupCtx, token, stableAbortCode("commit", err))
		return Acknowledgement{}, errors.Join(err, rollbackErr, abortErr)
	}
	c.engine.FinalizeActivation(context.WithoutCancel(ctx), token, set)
	return cloneCoordinatorAck(ack), nil
}

func (c *Coordinator) loadCommittedPublicationSet(
	ctx context.Context,
	requested ProviderCursor,
	ack Acknowledgement,
) (PublicationSet, error) {
	if ack.Cursor != requested || ack.Revisions.Desired == 0 {
		return PublicationSet{}, ErrIntegrity
	}
	set := PublicationSet{
		DesiredRevision: ack.Revisions.Desired,
		Domains:         make(map[Domain]PublicationCandidate, len(ack.Decisions)),
	}
	for domain := range ack.Decisions {
		if domain != DomainHTTP && domain != DomainStream {
			return PublicationSet{}, ErrIntegrity
		}
	}
	for _, domain := range []Domain{DomainHTTP, DomainStream} {
		decisions, found := ack.Decisions[domain]
		if !found {
			if revisionForCoordinatorDomain(ack.Revisions, domain) >= ack.Revisions.Desired {
				return PublicationSet{}, ErrIntegrity
			}
			continue
		}
		if revisionForCoordinatorDomain(ack.Revisions, domain) != ack.Revisions.Desired {
			return PublicationSet{}, ErrIntegrity
		}
		published, err := c.journal.LoadPublished(ctx, domain)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return PublicationSet{}, ErrIntegrity
			}
			return PublicationSet{}, err
		}
		if published.Artifact.Domain != domain ||
			published.Artifact.Revision != ack.Revisions.Desired ||
			!slices.Equal(published.Decisions, decisions) {
			return PublicationSet{}, ErrIntegrity
		}
		set.Domains[domain] = publicationCandidateFromPublished(published)
	}
	return set, nil
}

func publicationCandidateFromPublished(published PublishedGeneration) PublicationCandidate {
	return PublicationCandidate{
		Artifact:  published.Artifact,
		Snapshot:  published.Snapshot.Clone(),
		Closure:   slices.Clone(published.Closure),
		Decisions: slices.Clone(published.Decisions),
	}
}

func revisionForCoordinatorDomain(revisions RevisionSet, domain Domain) uint64 {
	switch domain {
	case DomainHTTP:
		return revisions.HTTP
	case DomainStream:
		return revisions.Stream
	default:
		return 0
	}
}

func cloneCoordinatorAck(ack Acknowledgement) Acknowledgement {
	clone := ack
	if ack.Decisions == nil {
		return clone
	}
	clone.Decisions = make(map[Domain][]ResourceDecision, len(ack.Decisions))
	for domain, decisions := range ack.Decisions {
		clone.Decisions[domain] = slices.Clone(decisions)
	}
	return clone
}

func stableAbortCode(phase string, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return phase + "-context-canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return phase + "-deadline-exceeded"
	default:
		return phase + "-failed"
	}
}
