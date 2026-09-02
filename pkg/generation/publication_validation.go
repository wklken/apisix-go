package generation

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxDecisionDiagnosticBytes = 2048

// ValidatePublicationSet checks that a publication set is an exact structural
// match for its apply ticket and that every domain candidate is internally
// consistent.
func ValidatePublicationSet(ticket ApplyTicket, set PublicationSet) error {
	if ticket.DesiredRevision == 0 || set.DesiredRevision != ticket.DesiredRevision ||
		!equalNormalizedDomains(ticket.RequiredDomains) ||
		len(set.Domains) != len(ticket.RequiredDomains) {
		return ErrIntegrity
	}
	for domain := range set.Domains {
		if !validPublicationDomain(domain) || !containsDomain(ticket.RequiredDomains, domain) {
			return ErrIntegrity
		}
	}
	for _, domain := range ticket.RequiredDomains {
		candidate, ok := set.Domains[domain]
		if !ok {
			return ErrIntegrity
		}
		if err := ValidatePublicationCandidate(domain, ticket.DesiredRevision, candidate); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePublicationCandidate checks the structural identity and closure
// invariants for one domain's publication candidate.
func ValidatePublicationCandidate(
	domain Domain,
	revision uint64,
	candidate PublicationCandidate,
) error {
	if !validPublicationDomain(domain) || revision == 0 ||
		candidate.Artifact.Domain != domain || candidate.Artifact.Revision != revision ||
		candidate.Snapshot.Revision() != revision ||
		candidate.Artifact.Digest != candidate.Snapshot.Digest() ||
		candidate.Artifact.Snapshot != candidate.Snapshot.SnapshotID() {
		return ErrIntegrity
	}
	closure := make(map[ResourceKey]struct{}, len(candidate.Closure))
	for _, key := range candidate.Closure {
		if !validResourceKey(key) || !slices.Contains(DomainsForResourceKind(key.Kind), domain) {
			return ErrInvalidClosure
		}
		if _, exists := closure[key]; exists {
			return ErrInvalidClosure
		}
		closure[key] = struct{}{}
	}
	resources := make(map[ResourceKey]struct{})
	for _, resource := range candidate.Snapshot.Resources() {
		if _, exists := closure[resource.Key]; !exists {
			return ErrInvalidClosure
		}
		resources[resource.Key] = struct{}{}
	}
	tombstones := make(map[ResourceKey]struct{})
	for _, tombstone := range candidate.Snapshot.Tombstones() {
		if _, exists := closure[tombstone.Key]; !exists {
			return ErrInvalidClosure
		}
		tombstones[tombstone.Key] = struct{}{}
	}
	decisions := make(map[ResourceKey]ResourceDecision, len(candidate.Decisions))
	for _, decision := range candidate.Decisions {
		if !validResourceKey(decision.Key) || decision.Code == "" || !utf8.ValidString(decision.Code) ||
			!validDisposition(decision.Disposition) || !validDecisionDiagnostic(decision.Diagnostic) ||
			(decision.Diagnostic != "" && !rejectedDisposition(decision.Disposition)) {
			return ErrInvalidClosure
		}
		if _, exists := decisions[decision.Key]; exists {
			return ErrInvalidClosure
		}
		decisions[decision.Key] = decision
	}
	if len(decisions) != len(closure) {
		return ErrInvalidClosure
	}
	for key := range closure {
		decision, exists := decisions[key]
		if !exists {
			return ErrInvalidClosure
		}
		_, resource := resources[key]
		_, deleted := tombstones[key]
		switch decision.Disposition {
		case DispositionPublished, DispositionLastGood:
			if !resource || deleted {
				return ErrInvalidClosure
			}
		case DispositionQuarantined, DispositionFailClosed:
			if resource || deleted {
				return ErrInvalidClosure
			}
		case DispositionDeleted:
			if resource || !deleted {
				return ErrInvalidClosure
			}
		default:
			return ErrInvalidClosure
		}
	}
	return nil
}

// DecisionDiagnostics returns safe, stable diagnostics from a provider
// acknowledgement. Providers use this single validation boundary before
// writing compiler-supplied diagnostics to their logs.
func DecisionDiagnostics(decisions map[Domain][]ResourceDecision) ([]string, error) {
	diagnostics := make([]string, 0)
	for _, domainDecisions := range decisions {
		for _, decision := range domainDecisions {
			if !validDecisionDiagnostic(decision.Diagnostic) ||
				(decision.Diagnostic != "" && !rejectedDisposition(decision.Disposition)) {
				return nil, ErrIntegrity
			}
			if decision.Diagnostic != "" {
				diagnostics = append(diagnostics, decision.Diagnostic)
			}
		}
	}
	slices.Sort(diagnostics)
	return slices.Compact(diagnostics), nil
}

func validDecisionDiagnostic(diagnostic string) bool {
	return len(diagnostic) <= maxDecisionDiagnosticBytes &&
		utf8.ValidString(diagnostic) &&
		strings.IndexFunc(diagnostic, unicode.IsControl) == -1
}

func rejectedDisposition(disposition ResourceDisposition) bool {
	return disposition == DispositionLastGood ||
		disposition == DispositionQuarantined ||
		disposition == DispositionFailClosed
}

// ValidatePublishedGeneration checks the structural identity and closure
// invariants for a previously published generation.
func ValidatePublishedGeneration(
	domain Domain,
	revision uint64,
	published PublishedGeneration,
) error {
	return ValidatePublicationCandidate(domain, revision, PublicationCandidate(published))
}

func equalNormalizedDomains(domains []Domain) bool {
	index := 0
	if index < len(domains) && domains[index] == DomainHTTP {
		index++
	}
	if index < len(domains) && domains[index] == DomainStream {
		index++
	}
	return index == len(domains)
}

func containsDomain(domains []Domain, want Domain) bool {
	return slices.Contains(domains, want)
}

func validPublicationDomain(domain Domain) bool {
	return domain == DomainHTTP || domain == DomainStream
}

func validResourceKey(key ResourceKey) bool {
	return key.Kind != "" && key.ID != "" && utf8.ValidString(key.Kind) && utf8.ValidString(key.ID)
}

func validDisposition(disposition ResourceDisposition) bool {
	switch disposition {
	case DispositionPublished, DispositionLastGood, DispositionQuarantined,
		DispositionFailClosed, DispositionDeleted:
		return true
	default:
		return false
	}
}
