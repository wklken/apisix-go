package compiler

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

type candidateDecision struct {
	decision  generation.ResourceDecision
	value     []byte
	tombstone *generation.Tombstone
}

var securitySensitiveKinds = map[string]struct{}{
	"routes": {}, "services": {}, "global_rules": {}, "plugin_configs": {},
	"plugin_metadata": {}, "plugins": {}, "ssls": {}, "secrets": {},
	"consumers": {}, "consumer_groups": {}, "stream_routes": {},
}

func buildDomainCandidateContext(
	ctx context.Context,
	domain generation.Domain,
	desired generation.Snapshot,
	input normalizedInput,
	issues []resourceIssue,
	previous generation.PublishedGeneration,
	hasPrevious bool,
	manifest *capability.Manifest,
) (generation.PublicationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return generation.PublicationCandidate{}, err
	}
	if domain != generation.DomainHTTP && domain != generation.DomainStream {
		return generation.PublicationCandidate{}, fmt.Errorf("%w: unknown domain %q", ErrInvalidInput, domain)
	}
	if hasPrevious && !validPublishedPredecessor(domain, previous) {
		return generation.PublicationCandidate{}, generation.ErrIntegrity
	}
	issueByKey := firstIssueByKey(issues)
	if domain == generation.DomainStream {
		for _, issue := range streamClientSSLIssues(input) {
			if err := ctx.Err(); err != nil {
				return generation.PublicationCandidate{}, err
			}
			if _, alreadyInvalid := issueByKey[issue.Key]; !alreadyInvalid {
				issueByKey[issue.Key] = issue
			}
		}
	}
	decisions := make(map[generation.ResourceKey]candidateDecision)
	for _, key := range domainKeys(domain, input) {
		if err := ctx.Err(); err != nil {
			return generation.PublicationCandidate{}, err
		}
		if tombstone, deleted := input.tombstones[key]; deleted {
			copy := tombstone
			decisions[key] = candidateDecision{
				decision: generation.ResourceDecision{
					Key:         key,
					Disposition: generation.DispositionDeleted,
					Code:        "explicit-delete",
				},
				tombstone: &copy,
			}
			continue
		}
		resource := input.resources[key]
		issue, invalid := issueByKey[key]
		if !invalid {
			decisions[key] = candidateDecision{
				decision: generation.ResourceDecision{
					Key:         key,
					Disposition: generation.DispositionPublished,
					Code:        "validated",
				},
				value: bytes.Clone(resource.raw),
			}
			continue
		}
		if issue.Code == "stream-client-ssl-deferred" {
			decisions[key] = candidateDecision{
				decision: generation.ResourceDecision{
					Key:         key,
					Disposition: generation.DispositionQuarantined,
					Code:        issue.Code,
				},
			}
			continue
		}
		if issue.Code == "dependency-missing" {
			if _, sensitive := securitySensitiveKinds[key.Kind]; sensitive && hasPrevious {
				if value, found := previous.Snapshot.Lookup(key); found {
					decisions[key] = candidateDecision{
						decision: generation.ResourceDecision{
							Key:         key,
							Disposition: generation.DispositionLastGood,
							Code:        issue.Code,
						},
						value: value,
					}
					continue
				}
			}
			decisions[key] = candidateDecision{
				decision: generation.ResourceDecision{
					Key:         key,
					Disposition: generation.DispositionPublished,
					Code:        "validated",
				},
				value: bytes.Clone(resource.raw),
			}
			continue
		}
		if _, sensitive := securitySensitiveKinds[key.Kind]; sensitive {
			if hasPrevious {
				if value, found := previous.Snapshot.Lookup(key); found {
					decisions[key] = candidateDecision{
						decision: generation.ResourceDecision{
							Key:         key,
							Disposition: generation.DispositionLastGood,
							Code:        issue.Code,
						},
						value: value,
					}
					continue
				}
			}
			decisions[key] = candidateDecision{
				decision: generation.ResourceDecision{
					Key:         key,
					Disposition: generation.DispositionFailClosed,
					Code:        issue.Code,
				},
			}
			continue
		}
		decisions[key] = candidateDecision{
			decision: generation.ResourceDecision{
				Key:         key,
				Disposition: generation.DispositionQuarantined,
				Code:        issue.Code,
			},
		}
	}

	if err := enforceEffectiveClosure(ctx, domain, desired.Revision(), decisions, manifest); err != nil {
		return generation.PublicationCandidate{}, err
	}
	candidate, err := assembleCandidate(domain, desired.Revision(), decisions)
	if err != nil {
		return generation.PublicationCandidate{}, err
	}
	if err := ctx.Err(); err != nil {
		return generation.PublicationCandidate{}, err
	}
	return candidate, nil
}

func validPublishedPredecessor(domain generation.Domain, previous generation.PublishedGeneration) bool {
	return previous.Artifact.Domain == domain &&
		previous.Artifact.Revision != 0 &&
		previous.Artifact.Revision == previous.Snapshot.Revision() &&
		previous.Artifact.Digest == previous.Snapshot.Digest() &&
		previous.Artifact.Snapshot == previous.Snapshot.SnapshotID()
}

func resourceUsesClientSSL(resource normalizedResource) bool {
	object, ok := resource.document.(map[string]any)
	if !ok {
		return false
	}
	usesClientSSL := func(raw any) bool {
		upstream, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		tls, ok := upstream["tls"].(map[string]any)
		if !ok {
			return false
		}
		id, valid := tlsReferenceID(tls["client_cert_id"])
		return valid && id != ""
	}
	if resource.key.Kind == "upstreams" && usesClientSSL(object) {
		return true
	}
	return usesClientSSL(object["upstream"])
}

func streamClientSSLIssues(input normalizedInput) []resourceIssue {
	issues := make([]resourceIssue, 0)
	for _, key := range input.keys() {
		if slices.Contains(generation.DomainsForResourceKind(key.Kind), generation.DomainStream) &&
			resourceUsesClientSSL(input.resources[key]) {
			issues = append(
				issues,
				newIssue(
					key,
					"stream-client-ssl-deferred",
					"stream client SSL requires explicit stream SSL domain support",
				),
			)
		}
	}
	return issues
}

func enforceEffectiveClosure(
	ctx context.Context,
	domain generation.Domain,
	revision uint64,
	decisions map[generation.ResourceKey]candidateDecision,
	manifest *capability.Manifest,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resources, tombstones := selectedValues(decisions)
	if err := ctx.Err(); err != nil {
		return err
	}
	effective, err := generation.NewSnapshot(revision, resources, tombstones)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	input, normalizationIssues, err := normalizeContext(ctx, effective)
	if err != nil {
		return err
	}
	validation, err := validateContext(ctx, input, manifest)
	if err != nil {
		return err
	}
	allIssues := append(normalizationIssues, validation.issuesForDomain(domain)...)
	if domain == generation.DomainStream {
		allIssues = append(allIssues, streamClientSSLIssues(input)...)
	}
	issueByKey := firstIssueByKey(compactIssues(allIssues))
	invalid := make(map[generation.ResourceKey]struct{}, len(issueByKey))
	queue := make([]generation.ResourceKey, 0, len(issueByKey))
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return err
		}
		issue, exists := issueByKey[key]
		if !exists || !invalidateSelectedDecision(decisions, key, issue) {
			continue
		}
		invalid[key] = struct{}{}
		queue = append(queue, key)
	}

	reverse := make(map[generation.ResourceKey][]generation.ResourceKey)
	for _, owner := range sortedGraphKeys(validation.graph.edges) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, selected := input.resources[owner]; !selected ||
			!slices.Contains(generation.DomainsForResourceKind(owner.Kind), domain) {
			continue
		}
		for _, dependency := range validation.graph.edges[owner] {
			if !validation.graph.supports(owner, dependency, domain) {
				continue
			}
			if _, selected := input.resources[dependency]; !selected {
				continue
			}
			reverse[dependency] = append(reverse[dependency], owner)
		}
	}
	for dependency, owners := range reverse {
		slices.SortFunc(owners, compareResourceKey)
		reverse[dependency] = slices.Compact(owners)
	}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dependency := queue[0]
		queue = queue[1:]
		for _, owner := range reverse[dependency] {
			if _, alreadyInvalid := invalid[owner]; alreadyInvalid {
				continue
			}
			current, selected := decisions[owner]
			if !selected || (current.decision.Disposition != generation.DispositionPublished &&
				current.decision.Disposition != generation.DispositionLastGood) {
				continue
			}
			current.decision.Disposition = generation.DispositionQuarantined
			current.decision.Code = "dependency-unavailable"
			current.value = nil
			decisions[owner] = current
			invalid[owner] = struct{}{}
			queue = append(queue, owner)
		}
	}
	return nil
}

func invalidateSelectedDecision(
	decisions map[generation.ResourceKey]candidateDecision,
	key generation.ResourceKey,
	issue resourceIssue,
) bool {
	current, exists := decisions[key]
	if !exists || (current.decision.Disposition != generation.DispositionPublished &&
		current.decision.Disposition != generation.DispositionLastGood) {
		return false
	}
	current.decision.Disposition = generation.DispositionFailClosed
	current.decision.Code = "effective-invalid"
	switch issue.Code {
	case "dependency-missing", "dependency-cycle":
		current.decision.Disposition = generation.DispositionQuarantined
		current.decision.Code = "dependency-unavailable"
	case "stream-client-ssl-deferred":
		current.decision.Disposition = generation.DispositionQuarantined
		current.decision.Code = issue.Code
	}
	current.value = nil
	decisions[key] = current
	return true
}

func assembleCandidate(
	domain generation.Domain,
	revision uint64,
	selected map[generation.ResourceKey]candidateDecision,
) (generation.PublicationCandidate, error) {
	resources, tombstones := selectedValues(selected)
	snapshot, err := generation.NewSnapshot(revision, resources, tombstones)
	if err != nil {
		return generation.PublicationCandidate{}, err
	}
	closure := make([]generation.ResourceKey, 0, len(selected))
	decisions := make([]generation.ResourceDecision, 0, len(selected))
	for key, selected := range selected {
		closure = append(closure, key)
		decisions = append(decisions, selected.decision)
	}
	slices.SortFunc(closure, compareResourceKey)
	slices.SortFunc(decisions, func(left, right generation.ResourceDecision) int {
		return compareResourceKey(left.Key, right.Key)
	})
	return generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: revision, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot, Closure: closure, Decisions: decisions,
	}, nil
}

func selectedValues(
	selected map[generation.ResourceKey]candidateDecision,
) ([]generation.Resource, []generation.Tombstone) {
	resources := make([]generation.Resource, 0, len(selected))
	tombstones := make([]generation.Tombstone, 0, len(selected))
	for key, item := range selected {
		switch item.decision.Disposition {
		case generation.DispositionPublished, generation.DispositionLastGood:
			resources = append(resources, generation.Resource{Key: key, Value: bytes.Clone(item.value)})
		case generation.DispositionDeleted:
			if item.tombstone != nil {
				tombstones = append(tombstones, *item.tombstone)
			}
		}
	}
	return resources, tombstones
}

func domainKeys(domain generation.Domain, input normalizedInput) []generation.ResourceKey {
	keys := make([]generation.ResourceKey, 0, len(input.resources)+len(input.tombstones))
	for key := range input.resources {
		if slices.Contains(generation.DomainsForResourceKind(key.Kind), domain) {
			keys = append(keys, key)
		}
	}
	for key := range input.tombstones {
		if slices.Contains(generation.DomainsForResourceKind(key.Kind), domain) {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, compareResourceKey)
	return keys
}

func firstIssueByKey(issues []resourceIssue) map[generation.ResourceKey]resourceIssue {
	ordered := slices.Clone(issues)
	sortIssues(ordered)
	result := make(map[generation.ResourceKey]resourceIssue, len(ordered))
	for _, issue := range ordered {
		current, exists := result[issue.Key]
		if !exists || dispositionIssuePriority(issue.Code) < dispositionIssuePriority(current.Code) {
			result[issue.Key] = issue
		}
	}
	return result
}

func dispositionIssuePriority(code string) int {
	switch code {
	case "dependency-missing", "dependency-cycle":
		return 2
	case "stream-client-ssl-deferred":
		return 1
	default:
		return 0
	}
}
