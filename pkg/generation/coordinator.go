package generation

import (
	"context"
	"slices"
	"sync"
)

// PublicationEngine compiles and atomically publishes detached runtime
// candidates for one desired-state revision.
type PublicationEngine interface {
	Publish(
		context.Context,
		ApplyTicket,
		Snapshot,
		map[Domain]PublishedGeneration,
	) (PublicationSet, error)
}

// Coordinator serializes provider updates and owns the in-memory desired and
// published heads used by the running process.
type Coordinator struct {
	engine PublicationEngine
	state  desiredState
	mu     sync.Mutex
}

func NewCoordinator(engine PublicationEngine) *Coordinator {
	return &Coordinator{engine: engine, state: newDesiredState()}
}

func (c *Coordinator) Apply(ctx context.Context, batch DesiredBatch) (Acknowledgement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Acknowledgement{}, err
	}

	candidate, err := c.state.candidate(batch)
	if err != nil {
		return Acknowledgement{}, err
	}
	if candidate.replay {
		return cloneAcknowledgement(candidate.acknowledgement), nil
	}

	previous := make(map[Domain]PublishedGeneration, len(candidate.ticket.RequiredDomains))
	for _, domain := range candidate.ticket.RequiredDomains {
		if published, ok := c.state.published[domain]; ok {
			previous[domain] = clonePublishedGeneration(published)
		}
	}
	set, err := c.engine.Publish(ctx, candidate.ticket, candidate.snapshot.Clone(), previous)
	if err != nil {
		return Acknowledgement{}, err
	}
	ack, err := c.state.commit(candidate, set)
	if err != nil {
		return Acknowledgement{}, err
	}
	return cloneAcknowledgement(ack), nil
}

func cloneAcknowledgement(ack Acknowledgement) Acknowledgement {
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

func clonePublishedGeneration(published PublishedGeneration) PublishedGeneration {
	clone := published
	clone.Snapshot = published.Snapshot.Clone()
	clone.Closure = slices.Clone(published.Closure)
	clone.Decisions = slices.Clone(published.Decisions)
	return clone
}
