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
	candidate, exists := prepared.attempt.Candidate(generation.DomainStream)
	if !exists {
		return nil
	}
	plan, err := prepared.planStreamPreparation(ctx, candidate)
	if err != nil {
		return err
	}
	routes := make([]streamruntime.PreparedRoute, 0, len(plan.routes))
	for _, planned := range plan.routes {
		if err := ctx.Err(); err != nil {
			return err
		}
		preparedRoute, err := prepared.materializePlannedStreamRoute(ctx, planned)
		if err != nil {
			return err
		}
		routes = append(routes, preparedRoute)
	}
	router, err := streamruntime.CompileRouter(ctx, streamruntime.CompileInput{
		Revision: candidate.Artifact.Revision,
		Routes:   routes,
	})
	if err != nil {
		return err
	}
	return prepared.attachStream(&StreamSnapshot{artifact: candidate.Artifact, router: router})
}
