package compiler

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type httpResourceSet struct {
	revision         uint64
	routes           []resource.Route
	services         map[string]resource.Service
	upstreams        map[string]resource.Upstream
	pluginConfigs    map[string]resource.PluginConfigRule
	globalRules      []resource.GlobalRule
	ssls             map[string]resource.SSL
	enabledPlugins   []string
	dynamicPlugins   bool
	consumerIDs      []string
	consumerGroupIDs []string
}

func decodeHTTPResourceSet(
	ctx context.Context,
	candidate generation.PublicationCandidate,
) (httpResourceSet, error) {
	if ctx == nil {
		return httpResourceSet{}, fmt.Errorf("%w: HTTP decode context is required", ErrInvalidInput)
	}
	if err := generation.ValidatePublicationCandidate(
		generation.DomainHTTP,
		candidate.Artifact.Revision,
		candidate,
	); err != nil {
		return httpResourceSet{}, err
	}
	input, issues, err := normalizeContext(ctx, candidate.Snapshot)
	if err != nil {
		return httpResourceSet{}, err
	}
	if len(issues) != 0 {
		return httpResourceSet{}, fmt.Errorf("%w: HTTP candidate is not normalized", ErrInvalidInput)
	}
	result := httpResourceSet{
		revision:      candidate.Artifact.Revision,
		services:      make(map[string]resource.Service),
		upstreams:     make(map[string]resource.Upstream),
		pluginConfigs: make(map[string]resource.PluginConfigRule),
		ssls:          make(map[string]resource.SSL),
	}
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return httpResourceSet{}, err
		}
		normalized := input.resources[key]
		switch key.Kind {
		case "routes":
			var value resource.Route
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.routes = append(result.routes, value)
		case "services":
			var value resource.Service
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.services[key.ID] = value
		case "upstreams":
			var value resource.Upstream
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			result.upstreams[key.ID] = value
		case "plugin_configs":
			var value resource.PluginConfigRule
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			result.pluginConfigs[key.ID] = value
		case "global_rules":
			var value resource.GlobalRule
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.globalRules = append(result.globalRules, value)
		case "ssls":
			var value resource.SSL
			if err := util.Parse(normalized.document, &value); err != nil {
				return httpResourceSet{}, httpResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.ssls[key.ID] = value
		case "plugins":
			result.dynamicPlugins = true
			for _, name := range sortedFactories(normalized.view.plugins) {
				entry, ok := normalized.view.plugins[name].(map[string]any)
				if !ok {
					return httpResourceSet{}, httpResourceDecodeError(key, ErrInvalidInput)
				}
				stream, _ := entry["stream"].(bool)
				if !stream {
					result.enabledPlugins = append(result.enabledPlugins, name)
				}
			}
		case "consumers":
			result.consumerIDs = append(result.consumerIDs, key.ID)
		case "consumer_groups":
			result.consumerGroupIDs = append(result.consumerGroupIDs, key.ID)
		}
	}
	slices.Sort(result.consumerIDs)
	slices.Sort(result.consumerGroupIDs)
	return result, nil
}

func httpResourceDecodeError(key generation.ResourceKey, err error) error {
	return fmt.Errorf("%w: decode HTTP %s/%s: %v", ErrInvalidInput, key.Kind, key.ID, err)
}

// HTTPSnapshot is the authority-free HTTP/TLS observation surface of one
// prepared generation. Activation and cleanup remain generation-owned.
type HTTPSnapshot struct {
	artifact  generation.GenerationArtifact
	handler   http.Handler
	tlsConfig *tls.Config
}

func (snapshot *HTTPSnapshot) Revision() uint64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.artifact.Revision
}

func (snapshot *HTTPSnapshot) Handler() http.Handler {
	if snapshot == nil {
		return nil
	}
	return snapshot.handler
}

func (snapshot *HTTPSnapshot) TLSConfig() *tls.Config {
	if snapshot == nil || snapshot.tlsConfig == nil {
		return nil
	}
	return snapshot.tlsConfig.Clone()
}
