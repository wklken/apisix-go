package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type PublishedViewOptions struct {
	DataEncryption data_encryption.Service
}

// PublishedView owns one immutable, in-memory publication. It never reads the
// journal, the legacy Store buckets, package globals, or mutable Store caches.
type PublishedView struct {
	published      generation.PublishedGeneration
	dataEncryption data_encryption.Service
	values         map[generation.ResourceKey][]byte
}

func NewPublishedView(
	published generation.PublishedGeneration,
	options PublishedViewOptions,
) (*PublishedView, error) {
	domain := published.Artifact.Domain
	if !validPublicationDomain(domain) || published.Artifact.Revision == 0 {
		return nil, generation.ErrIntegrity
	}
	if err := validatePublicationCandidate(
		domain,
		published.Artifact.Revision,
		generation.PublicationCandidate(published),
	); err != nil {
		return nil, err
	}
	cloned := clonePublishedGeneration(published)
	view := &PublishedView{
		published:      cloned,
		dataEncryption: options.DataEncryption,
		values:         make(map[generation.ResourceKey][]byte, len(cloned.Snapshot.Resources())),
	}
	for _, item := range cloned.Snapshot.Resources() {
		view.values[item.Key] = bytes.Clone(item.Value)
	}
	return view, nil
}

func (v *PublishedView) Published() generation.PublishedGeneration {
	if v == nil {
		return generation.PublishedGeneration{}
	}
	return clonePublishedGeneration(v.published)
}

func (v *PublishedView) Raw(kind, id string) ([]byte, bool) {
	if v == nil {
		return nil, false
	}
	value, found := v.values[generation.ResourceKey{Kind: kind, ID: id}]
	return bytes.Clone(value), found
}

func (v *PublishedView) Consumer(id string) (resource.Consumer, error) {
	if !v.httpTypedAccess() {
		return resource.Consumer{}, generation.ErrIntegrity
	}
	raw, found := v.Raw("consumers", id)
	if !found {
		return resource.Consumer{}, ErrNotFound
	}
	var consumer resource.Consumer
	if err := json.Unmarshal(raw, &consumer); err != nil {
		return resource.Consumer{}, err
	}
	v.decryptPluginConfigs(consumer.Plugins)
	consumer.ConfigDigest = sha256.Sum256(raw)
	return cloneConsumer(consumer), nil
}

func (v *PublishedView) ConsumerGroup(id string) (resource.ConsumerGroup, error) {
	if !v.httpTypedAccess() {
		return resource.ConsumerGroup{}, generation.ErrIntegrity
	}
	raw, found := v.Raw("consumer_groups", id)
	if !found {
		return resource.ConsumerGroup{}, ErrNotFound
	}
	var group resource.ConsumerGroup
	if err := json.Unmarshal(raw, &group); err != nil {
		return resource.ConsumerGroup{}, err
	}
	v.decryptPluginConfigs(group.Plugins)
	group.ConfigDigest = sha256.Sum256(raw)
	group.Plugins = clonePluginConfigs(group.Plugins)
	return group, nil
}

func (v *PublishedView) SSL(id string) (resource.SSL, error) {
	if !v.httpTypedAccess() {
		return resource.SSL{}, generation.ErrIntegrity
	}
	raw, found := v.Raw("ssls", id)
	if !found {
		return resource.SSL{}, ErrNotFound
	}
	ssl, err := ParseSSL(raw)
	if err != nil {
		return resource.SSL{}, err
	}
	return clonePublishedViewSSL(ssl), nil
}

func (v *PublishedView) Proto(id string) (resource.Proto, error) {
	if !v.httpTypedAccess() {
		return resource.Proto{}, generation.ErrIntegrity
	}
	raw, found := v.Raw("protos", id)
	if !found {
		return resource.Proto{}, ErrNotFound
	}
	return ParseProto(raw)
}

func (v *PublishedView) PluginMetadataRaw(id string) ([]byte, bool) {
	if !v.httpTypedAccess() {
		return nil, false
	}
	return v.Raw("plugin_metadata", id)
}

func (v *PublishedView) PluginMetadata(id string, target any) error {
	if !v.httpTypedAccess() {
		return generation.ErrIntegrity
	}
	raw, found := v.PluginMetadataRaw(id)
	if !found {
		return ErrNotFound
	}
	if !v.dataEncryption.Enabled() || !data_encryption.HasEncryptedPluginMetadata(id) {
		return json.Unmarshal(raw, target)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	v.dataEncryption.DecryptPluginMetadata(id, metadata)
	return util.Parse(metadata, target)
}

func (v *PublishedView) StreamRoutes() ([]resource.StreamRoute, error) {
	if v == nil || v.published.Artifact.Domain != generation.DomainStream {
		return nil, generation.ErrIntegrity
	}
	routes := make([]resource.StreamRoute, 0)
	for _, item := range v.published.Snapshot.Resources() {
		if item.Key.Kind != "stream_routes" {
			continue
		}
		var route resource.StreamRoute
		if err := json.Unmarshal(item.Value, &route); err != nil {
			return nil, fmt.Errorf("decode stream_routes/%q: %w", item.Key.ID, err)
		}
		v.decryptPluginConfigs(route.Plugins)
		if route.ID == "" {
			route.ID = item.Key.ID
		}
		routes = append(routes, cloneStreamRoute(route))
	}
	return cloneStreamRoutes(routes), nil
}

func (v *PublishedView) ConfigSnapshot() (*ConfigSnapshot, error) {
	if v == nil || v.published.Artifact.Domain != generation.DomainHTTP {
		return nil, generation.ErrIntegrity
	}
	snapshot := &ConfigSnapshot{
		generation:       v.published.Artifact.Revision,
		pluginMetadata:   map[string]map[string]any{},
		services:         map[string]resource.Service{},
		upstreams:        map[string]resource.Upstream{},
		pluginConfigs:    map[string]resource.PluginConfigRule{},
		ssls:             map[string]resource.SSL{},
		globalRulesByKey: map[string]resource.GlobalRule{},
	}
	var dynamicPluginRows []generation.Resource
	for _, item := range v.published.Snapshot.Resources() {
		switch item.Key.Kind {
		case "routes":
			var route resource.Route
			if err := json.Unmarshal(item.Value, &route); err != nil {
				return nil, fmt.Errorf("decode routes/%q: %w", item.Key.ID, err)
			}
			v.decryptPluginConfigs(route.Plugins)
			snapshot.routes = append(snapshot.routes, cloneRoute(route))
		case "global_rules":
			var rule resource.GlobalRule
			if err := json.Unmarshal(item.Value, &rule); err != nil {
				return nil, fmt.Errorf("decode global_rules/%q: %w", item.Key.ID, err)
			}
			v.decryptPluginConfigs(rule.Plugins)
			snapshot.globalRules = append(snapshot.globalRules, cloneGlobalRule(rule))
			snapshot.globalRulesByKey[item.Key.ID] = cloneGlobalRule(rule)
		case "plugin_metadata":
			var metadata map[string]any
			if err := v.PluginMetadata(item.Key.ID, &metadata); err != nil {
				return nil, fmt.Errorf("decode plugin_metadata/%q: %w", item.Key.ID, err)
			}
			if metadata == nil {
				return nil, fmt.Errorf(
					"decode plugin_metadata/%q: expected JSON object",
					item.Key.ID,
				)
			}
			snapshot.pluginMetadata[item.Key.ID] = cloneAnyMap(metadata)
		case "services":
			var service resource.Service
			if err := json.Unmarshal(item.Value, &service); err != nil {
				return nil, fmt.Errorf("decode services/%q: %w", item.Key.ID, err)
			}
			v.decryptPluginConfigs(service.Plugins)
			snapshot.services[item.Key.ID] = cloneService(service)
		case "upstreams":
			upstream, err := ParseUpstream(item.Value)
			if err != nil {
				return nil, fmt.Errorf("decode upstreams/%q: %w", item.Key.ID, err)
			}
			snapshot.upstreams[item.Key.ID] = cloneUpstream(upstream)
		case "plugin_configs":
			var rule resource.PluginConfigRule
			if err := json.Unmarshal(item.Value, &rule); err != nil {
				return nil, fmt.Errorf("decode plugin_configs/%q: %w", item.Key.ID, err)
			}
			v.decryptPluginConfigs(rule.Plugins)
			snapshot.pluginConfigs[item.Key.ID] = clonePluginConfigRule(rule)
		case "ssls":
			ssl, err := ParseSSL(item.Value)
			if err != nil {
				return nil, fmt.Errorf("decode ssls/%q: %w", item.Key.ID, err)
			}
			snapshot.ssls[item.Key.ID] = clonePublishedViewSSL(ssl)
		case "plugins":
			dynamicPluginRows = append(dynamicPluginRows, item)
		}
	}
	if len(dynamicPluginRows) > 1 ||
		len(dynamicPluginRows) == 1 && dynamicPluginRows[0].Key.ID != "plugins" {
		return nil, fmt.Errorf("dynamic plugin publication requires one plugins/plugins resource")
	}
	if len(dynamicPluginRows) == 1 {
		plugins, err := parseDynamicHTTPPlugins(dynamicPluginRows[0].Value)
		if err != nil {
			return nil, fmt.Errorf("decode plugins/plugins: %w", err)
		}
		snapshot.httpPlugins = append([]string(nil), plugins...)
		snapshot.dynamicPlugins = true
	}
	return snapshot, nil
}

func (v *PublishedView) decryptPluginConfigs(configs map[string]resource.PluginConfig) {
	for name, config := range configs {
		v.dataEncryption.DecryptPluginConfig(config, name)
	}
}

func (v *PublishedView) httpTypedAccess() bool {
	return v != nil && v.published.Artifact.Domain == generation.DomainHTTP
}

func clonePublishedViewSSL(ssl resource.SSL) resource.SSL {
	return cloneSSL(ssl)
}

func cloneStreamRoutes(routes []resource.StreamRoute) []resource.StreamRoute {
	cloned := make([]resource.StreamRoute, len(routes))
	for index, route := range routes {
		cloned[index] = cloneStreamRoute(route)
	}
	return cloned
}
