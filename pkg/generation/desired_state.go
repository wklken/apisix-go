package generation

import (
	"bytes"
	"crypto/sha256"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
)

type desiredState struct {
	snapshot        Snapshot
	cursor          ProviderCursor
	batchDigest     [32]byte
	acknowledgement Acknowledgement
	published       map[Domain]PublishedGeneration
}

type desiredCandidate struct {
	snapshot        Snapshot
	ticket          ApplyTicket
	batchDigest     [32]byte
	replay          bool
	acknowledgement Acknowledgement
}

func newDesiredState() desiredState {
	snapshot, err := NewSnapshot(0, nil, nil)
	if err != nil {
		panic(err)
	}
	return desiredState{snapshot: snapshot, published: make(map[Domain]PublishedGeneration, 2)}
}

func (s desiredState) candidate(batch DesiredBatch) (desiredCandidate, error) {
	batch = cloneDesiredBatch(batch)
	digest, err := desiredBatchDigest(batch)
	if err != nil {
		return desiredCandidate{}, err
	}
	if s.cursor == batch.Cursor {
		if s.batchDigest != digest || s.acknowledgement.Revisions.Desired == 0 {
			return desiredCandidate{}, ErrCursorConflict
		}
		return desiredCandidate{
			replay: true, acknowledgement: cloneAcknowledgement(s.acknowledgement),
		}, nil
	}
	if s.cursor.Provider != "" && s.cursor.Provider != batch.Cursor.Provider && !batch.ReplaceManaged {
		return desiredCandidate{}, ErrProviderConflict
	}
	if s.snapshot.Revision() == math.MaxUint64 {
		return desiredCandidate{}, ErrIntegrity
	}
	next, err := applyDesiredBatch(s.snapshot, batch)
	if err != nil {
		return desiredCandidate{}, err
	}
	return desiredCandidate{
		snapshot:    next,
		batchDigest: digest,
		ticket: ApplyTicket{
			DesiredRevision: next.Revision(),
			DesiredDigest:   next.Digest(),
			Cursor:          batch.Cursor,
			RequiredDomains: normalizeDomains(batch.RequiredDomains),
		},
	}, nil
}

func (s *desiredState) commit(
	candidate desiredCandidate,
	set PublicationSet,
) (Acknowledgement, error) {
	if candidate.replay || candidate.snapshot.Revision() != candidate.ticket.DesiredRevision ||
		candidate.snapshot.Digest() != candidate.ticket.DesiredDigest {
		return Acknowledgement{}, ErrIntegrity
	}
	if err := ValidatePublicationSet(candidate.ticket, set); err != nil {
		return Acknowledgement{}, err
	}

	revisions := s.acknowledgement.Revisions
	revisions.Desired = candidate.ticket.DesiredRevision
	decisions := make(map[Domain][]ResourceDecision, len(set.Domains))
	updated := make(map[Domain]PublishedGeneration, len(s.published)+len(set.Domains))
	for domain, published := range s.published {
		updated[domain] = clonePublishedGeneration(published)
	}
	for _, domain := range candidate.ticket.RequiredDomains {
		publication := set.Domains[domain]
		switch domain {
		case DomainHTTP:
			revisions.HTTP = candidate.ticket.DesiredRevision
		case DomainStream:
			revisions.Stream = candidate.ticket.DesiredRevision
		default:
			return Acknowledgement{}, ErrIntegrity
		}
		decisions[domain] = slices.Clone(publication.Decisions)
		updated[domain] = clonePublishedGeneration(PublishedGeneration(publication))
	}
	ack := Acknowledgement{
		Cursor: candidate.ticket.Cursor, Revisions: revisions, Decisions: decisions,
	}
	s.snapshot = candidate.snapshot.Clone()
	s.cursor = candidate.ticket.Cursor
	s.batchDigest = candidate.batchDigest
	s.acknowledgement = cloneAcknowledgement(ack)
	s.published = updated
	return ack, nil
}

func validateDesiredBatch(batch DesiredBatch) error {
	if batch.Cursor.Provider == "" || batch.Cursor.Revision == "" ||
		!utf8.ValidString(batch.Cursor.Provider) || !utf8.ValidString(batch.Cursor.Revision) {
		return ErrIntegrity
	}
	for _, mutation := range batch.Mutations {
		if !validResourceKey(mutation.Key) {
			return ErrIntegrity
		}
		switch mutation.Type {
		case MutationPut:
		case MutationDelete:
			if len(mutation.Value) != 0 {
				return ErrIntegrity
			}
		default:
			return ErrIntegrity
		}
	}
	for _, domain := range batch.RequiredDomains {
		if !validPublicationDomain(domain) {
			return ErrIntegrity
		}
	}
	domains := normalizeDomains(batch.RequiredDomains)
	if batch.ReplaceManaged {
		if !slices.Equal(domains, []Domain{DomainHTTP, DomainStream}) {
			return ErrIntegrity
		}
	} else if len(batch.Mutations) != 0 && len(domains) == 0 {
		return ErrIntegrity
	}
	return nil
}

func desiredBatchDigest(batch DesiredBatch) ([32]byte, error) {
	if err := validateDesiredBatch(batch); err != nil {
		return [32]byte{}, err
	}
	type mutationWire struct {
		Type  MutationType `json:"type"`
		Key   ResourceKey  `json:"key"`
		Value []byte       `json:"value"`
	}
	mutations := make([]mutationWire, 0, len(batch.Mutations))
	for _, mutation := range batch.Mutations {
		value := bytes.Clone(mutation.Value)
		if mutation.Type == MutationDelete {
			value = nil
		}
		mutations = append(mutations, mutationWire{Type: mutation.Type, Key: mutation.Key, Value: value})
	}
	encoded, err := json.Marshal(struct {
		Cursor          ProviderCursor `json:"cursor"`
		ReplaceManaged  bool           `json:"replace_managed"`
		Mutations       []mutationWire `json:"mutations"`
		RequiredDomains []Domain       `json:"required_domains"`
	}{
		Cursor: batch.Cursor, ReplaceManaged: batch.ReplaceManaged,
		Mutations: mutations, RequiredDomains: normalizeDomains(batch.RequiredDomains),
	})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func applyDesiredBatch(current Snapshot, batch DesiredBatch) (Snapshot, error) {
	nextRevision := current.Revision() + 1
	resources := make(map[ResourceKey][]byte, len(current.Resources()))
	for _, resource := range current.Resources() {
		resources[resource.Key] = bytes.Clone(resource.Value)
	}
	tombstones := make(map[ResourceKey]uint64, len(current.Tombstones()))
	for _, tombstone := range current.Tombstones() {
		tombstones[tombstone.Key] = tombstone.Revision
	}
	if batch.ReplaceManaged {
		for key := range resources {
			delete(resources, key)
			tombstones[key] = nextRevision
		}
	}
	for _, mutation := range batch.Mutations {
		switch mutation.Type {
		case MutationPut:
			resources[mutation.Key] = bytes.Clone(mutation.Value)
			delete(tombstones, mutation.Key)
		case MutationDelete:
			delete(resources, mutation.Key)
			tombstones[mutation.Key] = nextRevision
		default:
			return Snapshot{}, ErrIntegrity
		}
	}
	resourceList := make([]Resource, 0, len(resources))
	for key, value := range resources {
		resourceList = append(resourceList, Resource{Key: key, Value: bytes.Clone(value)})
	}
	tombstoneList := make([]Tombstone, 0, len(tombstones))
	for key, revision := range tombstones {
		tombstoneList = append(tombstoneList, Tombstone{Key: key, Revision: revision})
	}
	return NewSnapshot(nextRevision, resourceList, tombstoneList)
}

func normalizeDomains(domains []Domain) []Domain {
	result := make([]Domain, 0, 2)
	if slices.Contains(domains, DomainHTTP) {
		result = append(result, DomainHTTP)
	}
	if slices.Contains(domains, DomainStream) {
		result = append(result, DomainStream)
	}
	return result
}

func cloneDesiredBatch(batch DesiredBatch) DesiredBatch {
	clone := batch
	clone.Mutations = slices.Clone(batch.Mutations)
	for index := range clone.Mutations {
		clone.Mutations[index].Value = bytes.Clone(batch.Mutations[index].Value)
	}
	clone.RequiredDomains = slices.Clone(batch.RequiredDomains)
	return clone
}
