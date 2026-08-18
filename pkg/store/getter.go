package store

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
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

// GetPluginMetadataRaw returns the raw plugin_metadata bytes for id, or nil
// when the metadata is absent. It is intended for change detection; callers
// still decode with GetPluginMetadata when a change is observed.
func GetPluginMetadataRaw(id string) ([]byte, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	return s.GetFromBucket("plugin_metadata", []byte(id))
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

	return util.Parse(metadata, v)
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

func validateConfigResourcePut(bucket, id string, config []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(config, &object); err != nil {
		return fmt.Errorf("decode %s resource: %w", bucket, err)
	}
	if object == nil {
		return fmt.Errorf("decode %s resource: expected JSON object", bucket)
	}
	switch bucket {
	case "routes":
		if _, err := ParseRoute(config); err != nil {
			return fmt.Errorf("decode route %q: %w", id, err)
		}
	case "global_rules":
		if _, err := ParseGlobalRule(config); err != nil {
			return fmt.Errorf("decode global rule %q: %w", id, err)
		}
	}
	return nil
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
	resolver := data_encryption.NewResolver(true, keyring)
	for name, config := range configs {
		data_encryption.DecryptPluginConfigWithResolver(config, name, resolver)
	}
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

// sslCertificateIndex is an immutable snapshot of decoded frontend SSL
// certificates, published atomically whenever the ssls bucket changes.
type sslCertificateIndex struct {
	exact    map[string]sslIndexedCertificate
	wildcard []sslWildcardCertificate
}

type sslIndexedCertificate struct {
	id          string
	certificate *tls.Certificate
}

type sslWildcardCertificate struct {
	sslIndexedCertificate
	suffix string
}

func (s *Store) applySSLCertificateEvent(eventType EventType, id string, value []byte) {
	current := s.sslCerts.Load()
	next := cloneSSLCertificateIndex(current)
	switch eventType {
	case EventTypePut:
		ssl, err := ParseSSL(value)
		if err != nil {
			logger.Errorf("reject SSL resource %q: parse: %s", id, err)
			return
		}
		if ssl.Status == 0 {
			next.remove(id)
			break
		}
		certificate, err := tls.X509KeyPair([]byte(ssl.Cert), []byte(ssl.Key))
		if err != nil {
			logger.Errorf("reject SSL resource %q: load: %s", id, err)
			return
		}
		next.remove(id)
		for _, sni := range ssl.Snis {
			sni = strings.TrimSpace(sni)
			if sni == "" {
				continue
			}
			entry := sslIndexedCertificate{id: id, certificate: &certificate}
			if strings.HasPrefix(sni, "*.") {
				next.wildcard = append(next.wildcard, sslWildcardCertificate{
					sslIndexedCertificate: entry,
					suffix:                strings.ToLower(sni[1:]),
				})
			} else {
				next.exact[strings.ToLower(sni)] = entry
			}
		}
	case EventTypeDelete:
		next.remove(id)
	}
	s.sslCerts.Store(next)
}

func (index *sslCertificateIndex) remove(id string) {
	if index == nil {
		return
	}
	for sni, entry := range index.exact {
		if entry.id == id {
			delete(index.exact, sni)
		}
	}
	if len(index.wildcard) > 0 {
		kept := index.wildcard[:0]
		for _, entry := range index.wildcard {
			if entry.id != id {
				kept = append(kept, entry)
			}
		}
		index.wildcard = kept
	}
}

func cloneSSLCertificateIndex(index *sslCertificateIndex) *sslCertificateIndex {
	next := &sslCertificateIndex{exact: map[string]sslIndexedCertificate{}}
	if index == nil {
		return next
	}
	maps.Copy(next.exact, index.exact)
	next.wildcard = append(next.wildcard, index.wildcard...)
	return next
}

// GetSSLCertificateForSNI returns the decoded frontend certificate for
// serverName from the process-wide store, preferring exact SNI matches over
// wildcard matches.
func GetSSLCertificateForSNI(serverName string) (*tls.Certificate, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	return s.GetSSLCertificateForSNI(serverName)
}

// GetSSLCertificateForSNI returns the decoded frontend certificate for
// serverName, preferring exact SNI matches over wildcard matches.
// Certificates are decoded once at publication; handshakes only look up.
func (s *Store) GetSSLCertificateForSNI(serverName string) (*tls.Certificate, error) {
	serverName = strings.TrimSpace(serverName)
	index := s.sslCerts.Load()
	if index == nil {
		return nil, fmt.Errorf("no SSL certificate for SNI %q", serverName)
	}
	if entry, ok := index.exact[serverName]; ok {
		return entry.certificate, nil
	}
	lower := strings.ToLower(serverName)
	if lower != serverName {
		if entry, ok := index.exact[lower]; ok {
			return entry.certificate, nil
		}
	}
	for _, entry := range index.wildcard {
		if strings.HasSuffix(lower, entry.suffix) {
			return entry.certificate, nil
		}
	}
	return nil, fmt.Errorf("no SSL certificate for SNI %q", serverName)
}

// rebuildSSLCertificateIndex publishes a full index built from the ssls
// bucket. Invalid certificates are rejected: the last valid published index
// remains usable.
func (s *Store) rebuildSSLCertificateIndex() {
	next := &sslCertificateIndex{exact: map[string]sslIndexedCertificate{}}
	data, err := s.GetBucketData("ssls")
	if err != nil {
		logger.Errorf("publish SSL certificate index: %s", err)
		return
	}
	for _, value := range data {
		ssl, err := ParseSSL(value)
		if err != nil {
			logger.Errorf("reject SSL resource: parse: %s", err)
			return
		}
		if ssl.Status == 0 {
			continue
		}
		certificate, err := tls.X509KeyPair([]byte(ssl.Cert), []byte(ssl.Key))
		if err != nil {
			logger.Errorf("reject SSL resource %q: load: %s", ssl.ID, err)
			return
		}
		for _, sni := range ssl.Snis {
			sni = strings.TrimSpace(sni)
			if sni == "" {
				continue
			}
			entry := sslIndexedCertificate{id: ssl.ID, certificate: &certificate}
			if strings.HasPrefix(sni, "*.") {
				next.wildcard = append(next.wildcard, sslWildcardCertificate{
					sslIndexedCertificate: entry,
					suffix:                strings.ToLower(sni[1:]),
				})
			} else {
				next.exact[strings.ToLower(sni)] = entry
			}
		}
	}
	s.sslCerts.Store(next)
}

// ConfigSnapshot is an immutable generation of the route-build inputs:
// parsed routes, parsed global rules, and decoded plugin metadata. It is
// published once per store change; callers must not mutate it.
type ConfigSnapshot struct {
	generation     uint64
	routes         []resource.Route
	globalRules    []resource.GlobalRule
	pluginMetadata map[string]map[string]any
	quarantined    []ConfigQuarantine
}

// ConfigQuarantine identifies a legacy route or global-rule row that could
// not be decoded while the remainder of the immutable snapshot was published.
// Callers receive a copy from QuarantinedResources and may not mutate the
// snapshot's internal state.
type ConfigQuarantine struct {
	Bucket string
	ID     string
}

// Routes returns the parsed routes of this snapshot generation.
func (snap *ConfigSnapshot) Routes() []resource.Route {
	return snap.routes
}

// GlobalRules returns the parsed global rules of this snapshot generation.
func (snap *ConfigSnapshot) GlobalRules() []resource.GlobalRule {
	return snap.globalRules
}

// PluginMetadata returns the decoded plugin metadata for id. The boolean is
// false when the id has no decodable plugin metadata.
func (snap *ConfigSnapshot) PluginMetadata(id string) (map[string]any, bool) {
	metadata, ok := snap.pluginMetadata[id]
	return metadata, ok
}

// QuarantinedResources returns the stable bucket and bbolt key for each
// malformed legacy route/global-rule row skipped from this generation.
func (snap *ConfigSnapshot) QuarantinedResources() []ConfigQuarantine {
	return append([]ConfigQuarantine(nil), snap.quarantined...)
}

// GetConfigSnapshot returns the current route-build generation, rebuilding it
// once per routes/global-rules/plugin-metadata change.
func GetConfigSnapshot() (*ConfigSnapshot, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	return s.getConfigSnapshot()
}

func (s *Store) getConfigSnapshot() (*ConfigSnapshot, error) {
	generation := s.configGeneration.Load()
	if snapshot := s.configSnapshot.Load(); snapshot != nil && snapshot.generation == generation {
		return snapshot, nil
	}

	s.configSnapshotMu.Lock()
	defer s.configSnapshotMu.Unlock()
	for {
		generation := s.configGeneration.Load()
		if snapshot := s.configSnapshot.Load(); snapshot != nil && snapshot.generation == generation {
			return snapshot, nil
		}

		snapshot, err := s.buildConfigSnapshot(generation)
		if err != nil {
			return nil, err
		}
		if s.configGeneration.Load() != generation {
			continue
		}

		s.configSnapshot.Store(snapshot)
		return snapshot, nil
	}
}

func (s *Store) buildConfigSnapshot(generation uint64) (*ConfigSnapshot, error) {
	snapshot := &ConfigSnapshot{
		generation:     generation,
		pluginMetadata: map[string]map[string]any{},
	}

	entries, err := s.getBucketEntries("routes")
	if err != nil {
		return nil, err
	}
	if hook := s.afterConfigSnapshotBucketRead; hook != nil {
		hook("routes")
	}
	for _, entry := range entries {
		r, err := ParseRoute(entry.value)
		if err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "routes",
				ID:     entry.id,
			})
			continue
		}
		snapshot.routes = append(snapshot.routes, r)
	}

	entries, err = s.getBucketEntries("global_rules")
	if err != nil {
		return nil, err
	}
	if hook := s.afterConfigSnapshotBucketRead; hook != nil {
		hook("global_rules")
	}
	for _, entry := range entries {
		rule, err := ParseGlobalRule(entry.value)
		if err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "global_rules",
				ID:     entry.id,
			})
			continue
		}
		snapshot.globalRules = append(snapshot.globalRules, rule)
	}

	data, err := s.GetBucketData("plugin_metadata")
	if err != nil {
		return nil, err
	}
	if hook := s.afterConfigSnapshotBucketRead; hook != nil {
		hook("plugin_metadata")
	}
	for _, d := range data {
		id := metadataIDForDecodeError(d)
		var metadata map[string]any
		if err := decodePluginMetadata(d, id, &metadata); err != nil {
			continue
		}
		snapshot.pluginMetadata[id] = metadata
	}
	slices.SortFunc(snapshot.quarantined, func(left, right ConfigQuarantine) int {
		if comparison := strings.Compare(left.Bucket, right.Bucket); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.ID, right.ID)
	})
	return snapshot, nil
}

func metadataIDForDecodeError(config []byte) string {
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(config, &identity); err == nil && identity.ID != "" {
		return identity.ID
	}
	return "unknown"
}

// ProtoGeneration returns a counter that increments on every protos bucket
// change. Consumers can compare it against the generation they loaded from to
// skip re-reading the bucket when no proto resource changed.
func ProtoGeneration() int64 {
	if s == nil {
		return 0
	}
	return s.protosGeneration.Load()
}
