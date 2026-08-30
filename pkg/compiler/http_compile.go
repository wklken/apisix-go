package compiler

import (
	"context"
	"fmt"

	apisixid "github.com/wklken/apisix-go/pkg/apisix/id"
	"github.com/wklken/apisix-go/pkg/generation"
	routepkg "github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/tlsconfig"
)

func (prepared *PreparedGeneration) compileAndAttachHTTP(ctx context.Context) error {
	if prepared == nil || ctx == nil {
		return fmt.Errorf("%w: HTTP compiler owner and context are required", ErrInvalidInput)
	}
	candidate, exists := prepared.preparation.Candidate(generation.DomainHTTP)
	if !exists {
		return nil
	}
	plan, err := prepared.planHTTPPreparation(ctx, candidate)
	if err != nil {
		return err
	}
	plan.apiBreakerState, err = prepared.acquireHTTPAPIBreakerState(ctx)
	if err != nil {
		return err
	}
	nodeID := apisixid.Get(prepared.effective.Config.Apisix.ID)
	preparedRoutes, err := prepared.prepareHTTPRoutes(ctx, plan, nodeID)
	if err != nil {
		return err
	}
	notFound, err := routepkg.BuildPreparedNotFoundHandler(
		nodeID,
		preparedRoutes.notFound,
	)
	if err != nil {
		return err
	}
	routes := make([]routepkg.PreparedRoute, 0, len(preparedRoutes.routes))
	for _, compiled := range preparedRoutes.routes {
		routes = append(routes, routepkg.PreparedRoute{
			Route: compiled.planned.Route, Hosts: compiled.hosts, Handler: compiled.handler,
		})
	}
	router, err := routepkg.CompileHTTP(ctx, routepkg.CompileInput{
		Revision: candidate.Artifact.Revision,
		Routes:   routes, NotFound: notFound,
		StaticConfig: &prepared.effective.Config, Metadata: prepared.MetadataView(),
		PublicAPIRegistry:         plan.publicAPIRegistry,
		GraphQLProxyCacheRegistry: plan.purgeRegistry,
		ServerInfo:                prepared.observers.ServerInfo,
	})
	if err != nil {
		return err
	}
	var tlsSnapshot *tlsconfig.Snapshot
	if tlsconfig.FrontendEnabled(&prepared.effective.Config) {
		tlsSnapshot, err = tlsconfig.Compile(tlsconfig.Input{
			Config: &prepared.effective.Config, SSLs: plan.resources.ssls,
			TrustedClientCAPEM: prepared.trustedClientCAPEM,
		})
		if err != nil {
			return err
		}
	}
	return prepared.attachHTTP(&HTTPSnapshot{
		artifact:    candidate.Artifact,
		handler:     router.Handler(),
		tlsSnapshot: tlsSnapshot,
		quarantined: preparedRoutes.quarantined,
	})
}
