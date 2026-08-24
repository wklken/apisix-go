package compiler

import (
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/runtime"
	"go.yaml.in/yaml/v3"
)

func New(manifest *capability.Manifest, dependencies runtime.RuntimeDependencies) (*Compiler, error) {
	if manifest == nil {
		return nil, fmt.Errorf("%w: capability manifest is required", ErrInvalidInput)
	}
	if err := dependencies.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	encoded, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("%w: encode capability manifest: %v", ErrInvalidInput, err)
	}
	validatedManifest, err := capability.Parse(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	schemas, err := newSchemaSet(validatedManifest)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return &Compiler{manifest: validatedManifest, dependencies: dependencies, schemas: schemas}, nil
}

func (compiler *Compiler) PreparePublication(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (generation.PublicationSet, error) {
	if compiler == nil || compiler.manifest == nil {
		return generation.PublicationSet{}, fmt.Errorf("%w: compiler is not initialized", ErrInvalidInput)
	}
	if ctx == nil {
		return generation.PublicationSet{}, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return generation.PublicationSet{}, err
	}
	if ticket.DesiredRevision == 0 || ticket.DesiredRevision != desired.Revision() ||
		ticket.DesiredDigest != desired.Digest() {
		return generation.PublicationSet{}, generation.ErrIntegrity
	}
	if !validRequiredDomains(ticket.RequiredDomains) {
		return generation.PublicationSet{}, generation.ErrIntegrity
	}

	input, normalizationIssues, err := normalizeContext(ctx, desired)
	if err != nil {
		return generation.PublicationSet{}, err
	}
	for _, issue := range normalizationIssues {
		if err := ctx.Err(); err != nil {
			return generation.PublicationSet{}, err
		}
		if issue.Code == "unsupported-kind" {
			return generation.PublicationSet{}, fmt.Errorf(
				"%w: %s/%s is unsupported",
				ErrInvalidInput,
				issue.Key.Kind,
				issue.Key.ID,
			)
		}
	}
	validation, err := validateContext(ctx, input, compiler.manifest, compiler.schemas)
	if err != nil {
		return generation.PublicationSet{}, err
	}
	set := generation.PublicationSet{
		DesiredRevision: ticket.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(ticket.RequiredDomains)),
	}
	for _, domain := range ticket.RequiredDomains {
		if err := ctx.Err(); err != nil {
			return generation.PublicationSet{}, err
		}
		issues := compactIssues(append(slices.Clone(normalizationIssues), validation.issuesForDomain(domain)...))
		predecessor, found := previous[domain]
		candidate, err := buildDomainCandidateContext(
			ctx, domain, desired, input, issues, predecessor, found, compiler.manifest, compiler.schemas,
		)
		if err != nil {
			return generation.PublicationSet{}, fmt.Errorf("compile %s publication: %w", domain, err)
		}
		set.Domains[domain] = candidate
	}
	if len(set.Domains) != len(ticket.RequiredDomains) {
		return generation.PublicationSet{}, generation.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return generation.PublicationSet{}, err
	}
	return set, nil
}

func validRequiredDomains(domains []generation.Domain) bool {
	if !slices.IsSorted(domains) {
		return false
	}
	for index, domain := range domains {
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return false
		}
		if index > 0 && domains[index-1] == domain {
			return false
		}
	}
	return true
}
