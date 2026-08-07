package store

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

var ErrNotFound = fmt.Errorf("not found")

// FIXME: add a cache layer here, if the source data changed, del the cache at the same time

func GetPluginMetadata(id string, v any) error {
	if s == nil {
		return ErrNotFound
	}
	config, err := s.GetFromBucket("plugin_metadata", []byte(id))
	if err != nil {
		return err
	}
	return decodePluginMetadata(config, id, v)
}

func decodePluginMetadata(config []byte, id string, v any) error {
	keyring, enabled := data_encryption.Keyring()
	if !enabled || !data_encryption.HasEncryptedPluginMetadata(id) {
		return json.Unmarshal(config, v)
	}

	var metadata map[string]any
	if err := json.Unmarshal(config, &metadata); err != nil {
		return err
	}
	data_encryption.DecryptPluginMetadata(id, metadata, keyring)

	decoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, v)
}

func GetUpstream(id string) (resource.Upstream, error) {
	if s == nil {
		return resource.Upstream{}, ErrNotFound
	}
	config, err := s.GetFromBucket("upstreams", util.StringToBytes(id))
	if err != nil {
		return resource.Upstream{}, err
	}
	if config == nil {
		return resource.Upstream{}, ErrNotFound
	}

	return ParseUpstream(config)
}

func GetSSL(id string) (resource.SSL, error) {
	if s == nil {
		return resource.SSL{}, ErrNotFound
	}
	config, err := s.GetFromBucket("ssls", util.StringToBytes(id))
	if err != nil {
		return resource.SSL{}, err
	}
	if config == nil {
		return resource.SSL{}, ErrNotFound
	}

	return ParseSSL(config)
}

func GetStreamRoute(id string) (resource.StreamRoute, error) {
	if s == nil {
		return resource.StreamRoute{}, ErrNotFound
	}
	config, err := s.GetFromBucket("stream_routes", util.StringToBytes(id))
	if err != nil {
		return resource.StreamRoute{}, err
	}
	if config == nil {
		return resource.StreamRoute{}, ErrNotFound
	}

	return ParseStreamRoute(config)
}

func GetService(id string) (resource.Service, error) {
	if s == nil {
		return resource.Service{}, ErrNotFound
	}
	config, err := s.GetFromBucket("services", util.StringToBytes(id))
	if err != nil {
		return resource.Service{}, err
	}
	if config == nil {
		return resource.Service{}, ErrNotFound
	}

	return ParseService(config)
}

func GetConsumer(id string) (resource.Consumer, error) {
	if s == nil {
		return resource.Consumer{}, ErrNotFound
	}
	s.consumerMu.RLock()
	consumer, ok := s.consumerValues[id]
	s.consumerMu.RUnlock()
	if ok {
		return consumer, nil
	}
	config, err := s.GetFromBucket("consumers", util.StringToBytes(id))
	if err != nil {
		return resource.Consumer{}, err
	}
	if config == nil {
		return resource.Consumer{}, ErrNotFound
	}

	return ParseConsumer(config)
}

func GetConsumerGroup(id string) (resource.ConsumerGroup, error) {
	if s == nil {
		return resource.ConsumerGroup{}, ErrNotFound
	}
	config, err := s.GetFromBucket("consumer_groups", util.StringToBytes(id))
	if err != nil {
		return resource.ConsumerGroup{}, err
	}
	if config == nil {
		return resource.ConsumerGroup{}, ErrNotFound
	}

	return ParseConsumerGroup(config)
}

func GetPluginConfigRule(id string) (resource.PluginConfigRule, error) {
	if s == nil {
		return resource.PluginConfigRule{}, ErrNotFound
	}
	config, err := s.GetFromBucket("plugin_configs", util.StringToBytes(id))
	if err != nil {
		return resource.PluginConfigRule{}, err
	}
	if config == nil {
		return resource.PluginConfigRule{}, ErrNotFound
	}

	return ParsePluginConfigRule(config)
}

func GetProto(id string) (resource.Proto, error) {
	if s == nil {
		return resource.Proto{}, ErrNotFound
	}
	config, err := s.GetFromBucket("protos", util.StringToBytes(id))
	if err != nil {
		return resource.Proto{}, err
	}
	if config == nil {
		return resource.Proto{}, ErrNotFound
	}

	return ParseProto(config)
}

func ListRoutes() ([]resource.Route, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	data, err := s.GetBucketData("routes")
	if err != nil {
		return nil, err
	}
	var routes []resource.Route
	for _, d := range data {
		r, err := ParseRoute(d)
		if err != nil {
			return nil, fmt.Errorf("parse route %q: %w", routeIDForDecodeError(d), err)
		}
		routes = append(routes, r)
	}
	return routes, nil
}

func routeIDForDecodeError(config []byte) string {
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(config, &identity); err == nil && identity.ID != "" {
		return identity.ID
	}
	return "unknown"
}

func ListStreamRoutes() ([]resource.StreamRoute, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	data, err := s.GetBucketData("stream_routes")
	if err != nil {
		return nil, err
	}
	var routes []resource.StreamRoute
	for _, d := range data {
		route, err := ParseStreamRoute(d)
		if err != nil {
			return nil, fmt.Errorf("parse stream route error: %w", err)
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func ListSSLs() ([]resource.SSL, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	data, err := s.GetBucketData("ssls")
	if err != nil {
		return nil, err
	}
	ssls := make([]resource.SSL, 0, len(data))
	for _, value := range data {
		ssl, err := ParseSSL(value)
		if err != nil {
			return nil, fmt.Errorf("parse SSL resource: %w", err)
		}
		ssls = append(ssls, ssl)
	}
	return ssls, nil
}

func ListGlobalRules() ([]resource.GlobalRule, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	data, err := s.GetBucketData("global_rules")
	if err != nil {
		return nil, err
	}
	var rules []resource.GlobalRule
	for _, d := range data {
		r, err := ParseGlobalRule(d)
		if err != nil {
			return nil, fmt.Errorf("parse global rule %q: %w", globalRuleIDForDecodeError(d), err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

func globalRuleIDForDecodeError(config []byte) string {
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(config, &identity); err == nil && identity.ID != "" {
		return identity.ID
	}
	return "unknown"
}

func ParseRoute(config []byte) (resource.Route, error) {
	var r resource.Route
	err := json.Unmarshal(config, &r)
	if err != nil {
		return r, err
	}
	decryptPluginConfigs(r.Plugins)
	return r, nil
}

func ParseStreamRoute(config []byte) (resource.StreamRoute, error) {
	var route resource.StreamRoute
	if err := json.Unmarshal(config, &route); err != nil {
		return route, err
	}
	decryptPluginConfigs(route.Plugins)
	return route, nil
}

func ParseService(config []byte) (resource.Service, error) {
	var s resource.Service
	err := json.Unmarshal(config, &s)
	if err != nil {
		return s, err
	}
	decryptPluginConfigs(s.Plugins)
	return s, nil
}

func ParseUpstream(config []byte) (resource.Upstream, error) {
	var u resource.Upstream
	err := json.Unmarshal(config, &u)
	if err != nil {
		return u, err
	}
	return u, nil
}

func ParseSSL(config []byte) (resource.SSL, error) {
	var ssl resource.SSL
	if err := json.Unmarshal(config, &ssl); err != nil {
		return ssl, err
	}
	return ssl, nil
}

func ParseConsumer(config []byte) (resource.Consumer, error) {
	var c resource.Consumer
	err := json.Unmarshal(config, &c)
	if err != nil {
		return c, err
	}
	decryptPluginConfigs(c.Plugins)
	c.ConfigDigest = sha256.Sum256(config)
	return c, nil
}

func ParseConsumerGroup(config []byte) (resource.ConsumerGroup, error) {
	var c resource.ConsumerGroup
	err := json.Unmarshal(config, &c)
	if err != nil {
		return c, err
	}
	decryptPluginConfigs(c.Plugins)
	c.ConfigDigest = sha256.Sum256(config)
	return c, nil
}

func ParseGlobalRule(config []byte) (resource.GlobalRule, error) {
	var s resource.GlobalRule
	err := json.Unmarshal(config, &s)
	if err != nil {
		return s, err
	}
	decryptPluginConfigs(s.Plugins)
	return s, nil
}

func ParsePluginConfigRule(config []byte) (resource.PluginConfigRule, error) {
	var s resource.PluginConfigRule
	err := json.Unmarshal(config, &s)
	if err != nil {
		return s, err
	}
	decryptPluginConfigs(s.Plugins)
	return s, nil
}

func decryptPluginConfigs(configs map[string]resource.PluginConfig) {
	keyring, enabled := data_encryption.Keyring()
	if !enabled {
		return
	}
	values := make(map[string]any, len(configs))
	for name, value := range configs {
		values[name] = value
	}
	data_encryption.DecryptPluginConfigs(values, keyring)
}

func ParseProto(config []byte) (resource.Proto, error) {
	var p resource.Proto
	err := json.Unmarshal(config, &p)
	if err != nil {
		return p, err
	}
	return p, nil
}

func GetConsumerByPluginKey(pluginName string, key string) (resource.Consumer, error) {
	if s == nil {
		return resource.Consumer{}, ErrNotFound
	}
	return s.getConsumerByPluginKey(pluginName, key)
}

func (s *Store) getConsumerByPluginKey(pluginName, key string) (resource.Consumer, error) {
	directKey := fmt.Sprintf("%s:%s", pluginName, key)
	s.consumerMu.RLock()
	directID := append([]byte(nil), s.consumerKV[directKey]...)
	candidateIDs := make([]string, 0, len(s.consumerReferenceKV[pluginName]))
	for id := range s.consumerReferenceKV[pluginName] {
		candidateIDs = append(candidateIDs, id)
	}
	s.consumerMu.RUnlock()

	if len(directID) > 0 {
		consumer, err := s.resolveConsumerForPluginKey(string(directID), pluginName, key)
		if err != nil {
			return resource.Consumer{}, consumerCredentialLookupError(pluginName, err)
		}
		return consumer, nil
	}

	sort.Strings(candidateIDs)
	var resolveErr error
	for _, id := range candidateIDs {
		consumer, err := s.resolveConsumerForPluginKey(id, pluginName, key)
		if err != nil {
			resolveErr = err
			continue
		}
		return consumer, nil
	}
	if resolveErr != nil {
		return resource.Consumer{}, consumerCredentialLookupError(pluginName, resolveErr)
	}
	return resource.Consumer{}, ErrNotFound
}

func (s *Store) resolveConsumerForPluginKey(id, pluginName, key string) (resource.Consumer, error) {
	s.consumerMu.RLock()
	raw, ok := s.consumerValues[id]
	s.consumerMu.RUnlock()
	if !ok {
		return resource.Consumer{}, ErrNotFound
	}
	resolved, err := s.resolveConsumerPlugin(raw, pluginName)
	if err != nil {
		return resource.Consumer{}, err
	}
	resolvedKey, err := consumerPluginLookupKey(pluginName, resolved.Plugins[pluginName])
	if err != nil {
		return resource.Consumer{}, err
	}
	if resolvedKey != key {
		return resource.Consumer{}, ErrNotFound
	}
	return resolved, nil
}

func consumerCredentialLookupError(pluginName string, err error) error {
	return fmt.Errorf("%w: resolve %s consumer credentials: %v", ErrNotFound, pluginName, err)
}
