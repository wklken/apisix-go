package compiler

import (
	"context"
	"fmt"

	"github.com/wklken/apisix-go/pkg/generation"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func (prepared *PreparedGeneration) compileAndAttachStream(ctx context.Context) error {
	if prepared == nil || ctx == nil {
		return fmt.Errorf("%w: stream compiler owner and context are required", ErrInvalidInput)
	}
	candidate, exists := prepared.preparation.Candidate(generation.DomainStream)
	if !exists {
		return nil
	}
	plan, err := prepared.planStreamPreparation(ctx, candidate)
	if err != nil {
		return err
	}
	routes, err := prepared.materializePlannedStreamRoutes(ctx, plan.routes)
	if err != nil {
		return err
	}
	router, err := streamruntime.CompileRouter(ctx, streamruntime.CompileInput{
		Revision: candidate.Artifact.Revision,
		Routes:   routes,
		OnResult: prepared.observers.Stream,
	})
	if err != nil {
		return err
	}
	return prepared.attachStream(&StreamSnapshot{artifact: candidate.Artifact, router: router})
}
