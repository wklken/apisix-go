package compiler

import (
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

var errRecoverySnapshotInvalid = fmt.Errorf("%w: recovery snapshot validation failed", ErrInvalidInput)

// validateRecovery verifies committed publication state before it is exposed
// to a recovery attempt. Structural coverage belongs to generation so the
// compiler and secret registry share the same recovery boundary. This helper
// only performs compiler-owned raw admission and dependency validation, then
// returns an independent copy of the verified state.
func validateRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	committed map[generation.Domain]generation.PublishedGeneration,
	manifest *capability.Manifest,
	schemas *schemaSet,
) (map[generation.Domain]generation.PublishedGeneration, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if manifest == nil || schemas == nil {
		return nil, fmt.Errorf("%w: recovery schema inputs are required", ErrInvalidInput)
	}
	if len(committed) == 0 {
		return nil, generation.ErrIntegrity
	}
	if err := generation.ValidateRecoverySet(revisions, committed); err != nil {
		return nil, err
	}

	domains := recoveryDomains(revisions)
	for _, domain := range domains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		published := committed[domain]
		if err := validateRecoverySnapshot(ctx, domain, published.Snapshot, manifest, schemas); err != nil {
			return nil, err
		}
	}

	verified := make(map[generation.Domain]generation.PublishedGeneration, len(committed))
	for _, domain := range domains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		verified[domain] = clonePublishedGenerationForRecovery(committed[domain])
	}
	return verified, nil
}

func validateRecoverySnapshot(
	ctx context.Context,
	domain generation.Domain,
	snapshot generation.Snapshot,
	manifest *capability.Manifest,
	schemas *schemaSet,
) error {
	input, normalizationIssues, err := normalizeContext(ctx, snapshot)
	if err != nil {
		return err
	}
	for _, issue := range normalizationIssues {
		if recoveryIssueApplies(issue, domain) {
			return errRecoverySnapshotInvalid
		}
	}
	validation, err := validateContext(ctx, input, manifest, schemas)
	if err != nil {
		return err
	}
	if len(validation.issuesForDomain(domain)) != 0 {
		return errRecoverySnapshotInvalid
	}
	return nil
}

func recoveryIssueApplies(issue resourceIssue, domain generation.Domain) bool {
	if issue.Code == "unsupported-kind" {
		return true
	}
	return slices.Contains(generation.DomainsForResourceKind(issue.Key.Kind), domain)
}

func recoveryDomains(revisions generation.RevisionSet) []generation.Domain {
	domains := make([]generation.Domain, 0, 2)
	if revisions.HTTP != 0 {
		domains = append(domains, generation.DomainHTTP)
	}
	if revisions.Stream != 0 {
		domains = append(domains, generation.DomainStream)
	}
	return domains
}

func clonePublishedGenerationForRecovery(
	published generation.PublishedGeneration,
) generation.PublishedGeneration {
	clone := published
	clone.Snapshot = published.Snapshot.Clone()
	clone.Closure = slices.Clone(published.Closure)
	clone.Decisions = slices.Clone(published.Decisions)
	return clone
}
