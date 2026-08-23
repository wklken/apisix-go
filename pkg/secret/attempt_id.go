package secret

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"

	"github.com/wklken/apisix-go/pkg/generation"
)

// AttemptID identifies the immutable inputs owned by one secret materialization
// attempt. Candidate and recovery attempts use separate domains so that a
// desired-state publication can never alias a recovered publication.
type AttemptID [32]byte

const (
	candidateAttemptDomain = "apisix-go/secret-attempt/candidate/v1"
	recoveryAttemptDomain  = "apisix-go/secret-attempt/recovery/v1"
)

// CandidateAttemptID returns the identity of a desired publication attempt.
// A zero ID denotes an encoding failure.
func CandidateAttemptID(ticket generation.ApplyTicket, set generation.PublicationSet) (id AttemptID) {
	id, err := candidateAttemptIDChecked(ticket, set)
	if err != nil {
		return AttemptID{}
	}
	return id
}

// RecoveryAttemptID returns the identity of the verified published state used
// by a recovery attempt. A zero ID denotes an encoding failure.
func RecoveryAttemptID(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (id AttemptID) {
	id, err := recoveryAttemptIDChecked(revisions, published)
	if err != nil {
		return AttemptID{}
	}
	return id
}

func candidateAttemptIDChecked(
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptID, error) {
	encoded, err := encodeCandidateAttempt(ticket, set)
	if err != nil {
		return AttemptID{}, err
	}
	return sha256.Sum256(encoded), nil
}

func recoveryAttemptIDChecked(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptID, error) {
	encoded, err := encodeRecoveryAttempt(revisions, published)
	if err != nil {
		return AttemptID{}, err
	}
	return sha256.Sum256(encoded), nil
}

func encodeCandidateAttempt(
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) ([]byte, error) {
	encoder := newAttemptEncoder(candidateAttemptDomain)
	encoder.writeUint64(ticket.DesiredRevision)
	encoder.writeDigest(ticket.DesiredDigest)
	encoder.writeString(ticket.Cursor.Provider)
	encoder.writeString(ticket.Cursor.Revision)

	requiredDomains := append([]generation.Domain(nil), ticket.RequiredDomains...)
	sortDomains(requiredDomains)
	encoder.writeUint64(uint64(len(requiredDomains)))
	for _, domain := range requiredDomains {
		encoder.writeString(string(domain))
	}

	encoder.writeUint64(set.DesiredRevision)
	domains := sortedCandidateDomains(set.Domains)
	encoder.writeUint64(uint64(len(domains)))
	for _, domain := range domains {
		encoder.writeString(string(domain))
		if err := encoder.writePublicationCandidate(set.Domains[domain]); err != nil {
			return nil, err
		}
	}
	return encoder.bytes(), nil
}

func encodeRecoveryAttempt(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) ([]byte, error) {
	encoder := newAttemptEncoder(recoveryAttemptDomain)
	encoder.writeUint64(revisions.Desired)
	encoder.writeUint64(revisions.HTTP)
	encoder.writeUint64(revisions.Stream)

	domains := sortedPublishedDomains(published)
	encoder.writeUint64(uint64(len(domains)))
	for _, domain := range domains {
		encoder.writeString(string(domain))
		if err := encoder.writePublicationCandidate(generation.PublicationCandidate(published[domain])); err != nil {
			return nil, err
		}
	}
	return encoder.bytes(), nil
}

type attemptEncoder struct {
	buffer bytes.Buffer
}

func newAttemptEncoder(domain string) *attemptEncoder {
	encoder := &attemptEncoder{}
	encoder.writeString(domain)
	return encoder
}

func (encoder *attemptEncoder) bytes() []byte {
	return encoder.buffer.Bytes()
}

func (encoder *attemptEncoder) writeUint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = encoder.buffer.Write(encoded[:])
}

func (encoder *attemptEncoder) writeDigest(value [32]byte) {
	_, _ = encoder.buffer.Write(value[:])
}

func (encoder *attemptEncoder) writeBytes(value []byte) {
	encoder.writeUint64(uint64(len(value)))
	_, _ = encoder.buffer.Write(value)
}

func (encoder *attemptEncoder) writeString(value string) {
	encoder.writeBytes([]byte(value))
}

func (encoder *attemptEncoder) writePublicationCandidate(
	candidate generation.PublicationCandidate,
) error {
	encoder.writeString(string(candidate.Artifact.Domain))
	encoder.writeUint64(candidate.Artifact.Revision)
	encoder.writeDigest(candidate.Artifact.Digest)
	encoder.writeString(candidate.Artifact.Snapshot)

	encoder.writeUint64(candidate.Snapshot.Revision())
	encoder.writeDigest(candidate.Snapshot.Digest())
	canonicalSnapshot, err := candidate.Snapshot.CanonicalBytes()
	if err != nil {
		return err
	}
	encoder.writeBytes(canonicalSnapshot)

	closure := append([]generation.ResourceKey(nil), candidate.Closure...)
	sortResourceKeys(closure)
	encoder.writeUint64(uint64(len(closure)))
	for _, key := range closure {
		encoder.writeResourceKey(key)
	}

	decisions := append([]generation.ResourceDecision(nil), candidate.Decisions...)
	sortResourceDecisions(decisions)
	encoder.writeUint64(uint64(len(decisions)))
	for _, decision := range decisions {
		encoder.writeResourceKey(decision.Key)
		encoder.writeString(string(decision.Disposition))
		encoder.writeString(decision.Code)
	}
	return nil
}

func (encoder *attemptEncoder) writeResourceKey(key generation.ResourceKey) {
	encoder.writeString(key.Kind)
	encoder.writeString(key.ID)
}

func sortedCandidateDomains(domains map[generation.Domain]generation.PublicationCandidate) []generation.Domain {
	result := make([]generation.Domain, 0, len(domains))
	for domain := range domains {
		result = append(result, domain)
	}
	sortDomains(result)
	return result
}

func sortedPublishedDomains(domains map[generation.Domain]generation.PublishedGeneration) []generation.Domain {
	result := make([]generation.Domain, 0, len(domains))
	for domain := range domains {
		result = append(result, domain)
	}
	sortDomains(result)
	return result
}

func sortDomains(domains []generation.Domain) {
	sort.Slice(domains, func(left, right int) bool {
		return string(domains[left]) < string(domains[right])
	})
}

func sortResourceKeys(keys []generation.ResourceKey) {
	sort.Slice(keys, func(left, right int) bool {
		if byKind := strings.Compare(keys[left].Kind, keys[right].Kind); byKind != 0 {
			return byKind < 0
		}
		return keys[left].ID < keys[right].ID
	})
}

func sortResourceDecisions(decisions []generation.ResourceDecision) {
	sort.Slice(decisions, func(left, right int) bool {
		if byKind := strings.Compare(decisions[left].Key.Kind, decisions[right].Key.Kind); byKind != 0 {
			return byKind < 0
		}
		if byID := strings.Compare(decisions[left].Key.ID, decisions[right].Key.ID); byID != 0 {
			return byID < 0
		}
		if byDisposition := strings.Compare(
			string(decisions[left].Disposition),
			string(decisions[right].Disposition),
		); byDisposition != 0 {
			return byDisposition < 0
		}
		return decisions[left].Code < decisions[right].Code
	})
}
