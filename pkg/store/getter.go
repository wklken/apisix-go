package store

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/cacheutil"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
	bolt "go.etcd.io/bbolt"
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
		return cloneConsumer(consumer), nil
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
	return s.listStreamRoutes()
}

func (s *Store) listStreamRoutes() ([]resource.StreamRoute, error) {
	entries, err := s.getBucketEntries("stream_routes")
	if err != nil {
		return nil, err
	}
	lastGood := s.streamRouteLastGood.Load()
	routes := make([]resource.StreamRoute, 0, len(entries))
	published := make(map[string]resource.StreamRoute, len(entries))
	for _, entry := range entries {
		route, err := ParseStreamRoute(entry.value)
		if err != nil {
			if lastGood == nil {
				return nil, fmt.Errorf("parse stream route %q: %w", entry.id, err)
			}
			prev, ok := (*lastGood)[entry.id]
			if !ok {
				return nil, fmt.Errorf("parse stream route %q: %w", entry.id, err)
			}
			route = cloneStreamRoute(prev)
		}
		if route.ID == "" {
			route.ID = entry.id
		}
		routes = append(routes, route)
		published[entry.id] = cloneStreamRoute(route)
	}
	s.streamRouteLastGood.Store(&published)
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
	case "services":
		if _, err := ParseService(config); err != nil {
			return fmt.Errorf("decode service %q: %w", id, err)
		}
	case "upstreams":
		if _, err := ParseUpstream(config); err != nil {
			return fmt.Errorf("decode upstream %q: %w", id, err)
		}
	case "plugin_configs":
		if _, err := ParsePluginConfigRule(config); err != nil {
			return fmt.Errorf("decode plugin config %q: %w", id, err)
		}
	case "stream_routes":
		if _, err := ParseStreamRoute(config); err != nil {
			return fmt.Errorf("decode stream route %q: %w", id, err)
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
	for {
		consumerGeneration := s.consumerGeneration.Load()
		secretGeneration := s.secretGeneration.Load()
		snapshot, err := s.getConsumerLookupSnapshot(consumerGeneration, secretGeneration, pluginName)
		if consumerGeneration != s.consumerGeneration.Load() || secretGeneration != s.secretGeneration.Load() {
			continue
		}
		if err != nil {
			return resource.Consumer{}, err
		}
		owner, ok := snapshot.owners[key]
		if !ok {
			return resource.Consumer{}, ErrNotFound
		}
		consumer, err := s.resolveConsumerForPluginKey(owner, pluginName, key)
		if consumerGeneration != s.consumerGeneration.Load() || secretGeneration != s.secretGeneration.Load() {
			continue
		}
		if err != nil {
			return resource.Consumer{}, consumerCredentialLookupError(pluginName, err)
		}
		return consumer, nil
	}
}

const (
	consumerLookupCacheCapacity = 4096
	consumerLookupCacheTTL      = 5 * time.Second
	consumerLookupErrorTTL      = time.Second
)

type consumerLookupSnapshot struct {
	owners map[string]string
	err    error
}

func (s *Store) getConsumerLookupSnapshot(
	consumerGeneration, secretGeneration uint64,
	pluginName string,
) (consumerLookupSnapshot, error) {
	cacheKey := fmt.Sprintf("%d\x00%d\x00%s", consumerGeneration, secretGeneration, pluginName)
	cache := s.getConsumerLookupCache()
	if cached, ok := cache.Get(cacheKey); ok {
		return cached, cached.err
	}
	value, _, _ := s.consumerLookupGroup.Do(cacheKey, func() (any, error) {
		if cached, ok := cache.Get(cacheKey); ok {
			return cached, nil
		}
		resolved, cacheFailure := s.buildConsumerLookupSnapshot(pluginName)
		if resolved.err == nil {
			cache.Set(cacheKey, resolved, consumerLookupCacheTTL)
		} else if cacheFailure {
			cache.Set(cacheKey, resolved, consumerLookupErrorTTL)
		}
		return resolved, nil
	})
	resolved := value.(consumerLookupSnapshot)
	return resolved, resolved.err
}

func (s *Store) getConsumerLookupCache() *cacheutil.BoundedTTLMap[consumerLookupSnapshot] {
	s.consumerLookupCacheMu.Lock()
	defer s.consumerLookupCacheMu.Unlock()
	if s.consumerLookupCache == nil {
		s.consumerLookupCache = cacheutil.NewBoundedTTLMap[consumerLookupSnapshot](
			consumerLookupCacheCapacity,
			time.Now,
		)
	}
	return s.consumerLookupCache
}

type consumerLookupCandidate struct {
	id        string
	lookupKey string
}

func (s *Store) buildConsumerLookupSnapshot(pluginName string) (consumerLookupSnapshot, bool) {
	s.consumerMu.RLock()
	candidates := make([]consumerLookupCandidate, 0)
	for id, consumer := range s.consumerValues {
		config, ok := consumer.Plugins[pluginName]
		if !ok {
			continue
		}
		lookupKey, err := consumerPluginLookupKey(pluginName, config)
		if err != nil {
			s.consumerMu.RUnlock()
			return consumerLookupSnapshot{err: consumerCredentialLookupError(pluginName, err)}, true
		}
		candidates = append(candidates, consumerLookupCandidate{id: id, lookupKey: lookupKey})
	}
	s.consumerMu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].id < candidates[j].id })
	owners := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		resolvedKey := candidate.lookupKey
		if isConsumerSecretReference(resolvedKey) {
			var err error
			resolvedKey, err = s.resolveConsumerSecretString(resolvedKey)
			if err != nil {
				cacheFailure := strings.HasPrefix(candidate.lookupKey, managedSecretPrefix)
				return consumerLookupSnapshot{err: consumerCredentialLookupError(pluginName, err)}, cacheFailure
			}
		}
		if owner := owners[resolvedKey]; owner != "" && owner != candidate.id {
			return consumerLookupSnapshot{err: fmt.Errorf(
				"%w: %s credential matches multiple consumers",
				ErrNotFound,
				pluginName,
			)}, true
		}
		owners[resolvedKey] = candidate.id
	}
	return consumerLookupSnapshot{owners: owners}, false
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

// SSLCertificateConfig contains the immutable TLS material selected for an
// SNI. ClientCAs is nil when the SSL resource does not configure resource-level
// client certificate verification.
type SSLCertificateConfig struct {
	Certificate *tls.Certificate
	ClientCAs   *x509.CertPool
	ClientDepth int
}

type sslIndexedCertificate struct {
	id          string
	certificate *tls.Certificate
	clientCAs   *x509.CertPool
	clientDepth int
}

type sslWildcardCertificate struct {
	sslIndexedCertificate
	suffix string
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

// GetSSLCertificateConfigForSNI returns the decoded frontend certificate and
// any resource-level client CA selected for serverName.
func GetSSLCertificateConfigForSNI(serverName string) (*SSLCertificateConfig, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	return s.GetSSLCertificateConfigForSNI(serverName)
}

// GetSSLCertificateForSNI returns the decoded frontend certificate for
// serverName, preferring exact SNI matches over wildcard matches.
// Certificates are decoded once at publication; handshakes only look up.
func (s *Store) GetSSLCertificateForSNI(serverName string) (*tls.Certificate, error) {
	selected, err := s.GetSSLCertificateConfigForSNI(serverName)
	if err != nil {
		return nil, err
	}
	return selected.Certificate, nil
}

// GetSSLCertificateConfigForSNI returns the decoded frontend certificate and
// any resource-level client CA selected for serverName.
func (s *Store) GetSSLCertificateConfigForSNI(serverName string) (*SSLCertificateConfig, error) {
	serverName = strings.TrimSpace(serverName)
	index := s.sslCerts.Load()
	if index == nil {
		return nil, fmt.Errorf("no SSL certificate for SNI %q", serverName)
	}
	var selected sslIndexedCertificate
	var found bool
	if entry, ok := index.exact[serverName]; ok {
		selected, found = entry, true
	}
	lower := strings.ToLower(serverName)
	if !found && lower != serverName {
		if entry, ok := index.exact[lower]; ok {
			selected, found = entry, true
		}
	}
	if !found {
		for _, entry := range index.wildcard {
			if wildcardSSNIMatches(lower, entry.suffix) {
				selected, found = entry.sslIndexedCertificate, true
				break
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no SSL certificate for SNI %q", serverName)
	}
	return &SSLCertificateConfig{
		Certificate: selected.certificate,
		ClientCAs:   selected.clientCAs,
		ClientDepth: selected.clientDepth,
	}, nil
}

// rebuildSSLCertificateIndex publishes a full index built from the ssls
// bucket. Invalid certificates are rejected: the last valid published index
// remains usable.
func (s *Store) rebuildSSLCertificateIndex() {
	next := &sslCertificateIndex{exact: map[string]sslIndexedCertificate{}}
	entries := make([]bucketEntry, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("ssls"))
		if bucket == nil {
			return errBucketNotFound
		}
		return bucket.ForEach(func(id, value []byte) error {
			entries = append(entries, bucketEntry{
				id:    string(bytes.Clone(id)),
				value: bytes.Clone(value),
			})
			return nil
		})
	})
	if err != nil {
		logger.Errorf("publish SSL certificate index: %s", err)
		return
	}
	for _, entry := range entries {
		ssl, err := ParseSSL(entry.value)
		if err != nil {
			logger.Errorf("skip invalid persisted SSL resource %q: parse: %s", entry.id, err)
			continue
		}
		if ssl.Status == 0 {
			continue
		}
		if !isFrontendSSL(ssl) {
			continue
		}
		certificate, err := buildSSLCertificateConfig(ssl)
		if err != nil {
			logger.Errorf("skip invalid persisted SSL resource %q: load: %s", entry.id, err)
			continue
		}
		id := entry.id
		if id == "" {
			id = ssl.ID
		}
		for _, sni := range sslSNIs(ssl) {
			sni = normalizeSSLSNI(sni)
			if sni == "" {
				continue
			}
			indexed := sslIndexedCertificate{
				id:          id,
				certificate: certificate.Certificate,
				clientCAs:   certificate.ClientCAs,
				clientDepth: certificate.ClientDepth,
			}
			if strings.HasPrefix(strings.ToLower(sni), "*.") {
				next.wildcard = append(next.wildcard, sslWildcardCertificate{
					sslIndexedCertificate: indexed,
					suffix:                strings.ToLower(sni[1:]),
				})
			} else {
				next.exact[strings.ToLower(sni)] = indexed
			}
		}
	}
	s.sslCerts.Store(next)
}

var configSnapshotBuckets = []string{
	"routes",
	"global_rules",
	"plugin_metadata",
	"services",
	"upstreams",
	"plugin_configs",
	"ssls",
	"plugins",
}

func isFrontendSSL(ssl resource.SSL) bool {
	return ssl.Type == "" || ssl.Type == "server"
}

func sslSNIs(ssl resource.SSL) []string {
	if len(ssl.Snis) > 0 {
		return ssl.Snis
	}
	if ssl.Sni != "" {
		return []string{ssl.Sni}
	}
	return nil
}

func normalizeSSLSNI(sni string) string {
	sni = strings.TrimSpace(sni)
	return strings.TrimSuffix(sni, ".")
}

func wildcardSSNIMatches(serverName, suffix string) bool {
	if !strings.HasSuffix(serverName, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(serverName, suffix)
	return prefix != "" && !strings.Contains(prefix, ".")
}

func buildSSLCertificateConfig(ssl resource.SSL) (*SSLCertificateConfig, error) {
	certificate, err := tls.X509KeyPair([]byte(ssl.Cert), []byte(ssl.Key))
	if err != nil {
		return nil, err
	}
	clientCAs, err := parseSSLClientCAs(ssl.Client)
	if err != nil {
		return nil, err
	}
	clientDepth := 0
	if ssl.Client != nil {
		clientDepth = ssl.Client.Depth
	}
	return &SSLCertificateConfig{
		Certificate: &certificate,
		ClientCAs:   clientCAs,
		ClientDepth: clientDepth,
	}, nil
}

func parseSSLClientCAs(client *resource.SSLClient) (*x509.CertPool, error) {
	if client == nil {
		return nil, nil
	}
	if client.Depth != 1 {
		return nil, fmt.Errorf("unsupported SSL client depth %d; only depth 1 is supported", client.Depth)
	}
	if len(client.SkipMTLSURIRegex) > 0 {
		return nil, fmt.Errorf("unsupported SSL client skip_mtls_uri_regex")
	}
	ca := strings.TrimSpace(client.CA)
	if ca == "" {
		return nil, fmt.Errorf("SSL client.ca must not be empty")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca)) {
		return nil, fmt.Errorf("SSL client.ca contains no certificates")
	}
	return pool, nil
}

type dynamicPlugin struct {
	Name   string `json:"name"`
	Stream bool   `json:"stream"`
}

func decodeDynamicPluginList(value []byte) ([]dynamicPlugin, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	var plugins []dynamicPlugin
	if err := decoder.Decode(&plugins); err != nil {
		return nil, err
	}
	if plugins == nil {
		return nil, fmt.Errorf("expected JSON array")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	for index, plugin := range plugins {
		if plugin.Name == "" {
			return nil, fmt.Errorf("plugin[%d].name must not be empty", index)
		}
	}
	return plugins, nil
}

func validateDynamicPluginList(value []byte) error {
	_, err := decodeDynamicPluginList(value)
	if err != nil {
		return fmt.Errorf("reject dynamic plugin list: %w", err)
	}
	return nil
}

func parseDynamicHTTPPlugins(value []byte) ([]string, error) {
	plugins, err := decodeDynamicPluginList(value)
	if err != nil {
		return nil, err
	}
	httpPlugins := make([]string, 0, len(plugins))
	for _, plugin := range plugins {
		if !plugin.Stream {
			httpPlugins = append(httpPlugins, plugin.Name)
		}
	}
	return httpPlugins, nil
}

func resolveDynamicHTTPPlugins(entries []bucketEntry, previous *ConfigSnapshot) ([]string, bool, error) {
	if len(entries) == 0 {
		return nil, false, nil
	}
	if len(entries) == 1 && entries[0].id == "plugins" {
		httpPlugins, err := parseDynamicHTTPPlugins(entries[0].value)
		if err == nil {
			return httpPlugins, true, nil
		}
		if lastGood, ok := lastGoodHTTPPlugins(previous); ok {
			return lastGood, true, nil
		}
		return nil, false, fmt.Errorf("parse dynamic plugin list: %w", err)
	}
	if lastGood, ok := lastGoodHTTPPlugins(previous); ok {
		return lastGood, true, nil
	}
	if len(entries) > 1 {
		return nil, false, fmt.Errorf("dynamic plugin bucket contains %d entries, want one", len(entries))
	}
	return nil, false, fmt.Errorf("dynamic plugin bucket key %q, want plugins", entries[0].id)
}

func lastGoodSSL(previous *ConfigSnapshot, id string) (resource.SSL, bool) {
	if previous == nil {
		return resource.SSL{}, false
	}
	ssl, err := previous.GetSSL(id)
	return ssl, err == nil
}

func lastGoodGlobalRule(previous *ConfigSnapshot, id string) (resource.GlobalRule, bool) {
	if previous == nil {
		return resource.GlobalRule{}, false
	}
	for _, rule := range previous.globalRules {
		if rule.ID == id {
			return cloneGlobalRule(rule), true
		}
	}
	return resource.GlobalRule{}, false
}

func lastGoodHTTPPlugins(previous *ConfigSnapshot) ([]string, bool) {
	if previous == nil || !previous.dynamicPlugins {
		return nil, false
	}
	return append([]string(nil), previous.httpPlugins...), true
}

// ConfigSnapshot is an immutable generation of the HTTP route-build inputs.
// It is constructed from one bbolt read transaction and published once per
// Store generation. Callers receive cloned values from every accessor.
type ConfigSnapshot struct {
	generation     uint64
	routes         []resource.Route
	globalRules    []resource.GlobalRule
	pluginMetadata map[string]map[string]any
	services       map[string]resource.Service
	upstreams      map[string]resource.Upstream
	pluginConfigs  map[string]resource.PluginConfigRule
	ssls           map[string]resource.SSL
	httpPlugins    []string
	dynamicPlugins bool
	quarantined    []ConfigQuarantine
}

// ConfigQuarantine identifies a legacy configuration row that could not be
// decoded while the remainder of the immutable snapshot was published.
// Callers receive a copy from QuarantinedResources and may not mutate the
// snapshot's internal state.
type ConfigQuarantine struct {
	Bucket string
	ID     string
}

// Routes returns the parsed routes of this snapshot generation.
func (snap *ConfigSnapshot) Routes() []resource.Route {
	if snap == nil {
		return nil
	}
	routes := make([]resource.Route, len(snap.routes))
	for index, route := range snap.routes {
		routes[index] = cloneRoute(route)
	}
	return routes
}

// GlobalRules returns the parsed global rules of this snapshot generation.
func (snap *ConfigSnapshot) GlobalRules() []resource.GlobalRule {
	if snap == nil {
		return nil
	}
	rules := make([]resource.GlobalRule, len(snap.globalRules))
	for index, rule := range snap.globalRules {
		rules[index] = cloneGlobalRule(rule)
	}
	return rules
}

// PluginMetadata returns the decoded plugin metadata for id. The boolean is
// false when the id has no decodable plugin metadata.
func (snap *ConfigSnapshot) PluginMetadata(id string) (map[string]any, bool) {
	if snap == nil {
		return nil, false
	}
	metadata, ok := snap.pluginMetadata[id]
	if !ok {
		return nil, false
	}
	return cloneAnyMap(metadata), true
}

// GetService returns a cloned service from this generation.
func (snap *ConfigSnapshot) GetService(id string) (resource.Service, error) {
	if snap == nil {
		return resource.Service{}, ErrNotFound
	}
	service, ok := snap.services[id]
	if !ok {
		return resource.Service{}, ErrNotFound
	}
	return cloneService(service), nil
}

// GetUpstream returns a cloned upstream from this generation.
func (snap *ConfigSnapshot) GetUpstream(id string) (resource.Upstream, error) {
	if snap == nil {
		return resource.Upstream{}, ErrNotFound
	}
	upstream, ok := snap.upstreams[id]
	if !ok {
		return resource.Upstream{}, ErrNotFound
	}
	return cloneUpstream(upstream), nil
}

// GetPluginConfigRule returns a cloned plugin-config rule from this
// generation.
func (snap *ConfigSnapshot) GetPluginConfigRule(id string) (resource.PluginConfigRule, error) {
	if snap == nil {
		return resource.PluginConfigRule{}, ErrNotFound
	}
	rule, ok := snap.pluginConfigs[id]
	if !ok {
		return resource.PluginConfigRule{}, ErrNotFound
	}
	return clonePluginConfigRule(rule), nil
}

// GetSSL returns a cloned SSL resource from this generation.
func (snap *ConfigSnapshot) GetSSL(id string) (resource.SSL, error) {
	if snap == nil {
		return resource.SSL{}, ErrNotFound
	}
	ssl, ok := snap.ssls[id]
	if !ok {
		return resource.SSL{}, ErrNotFound
	}
	return cloneSSL(ssl), nil
}

// HTTPPlugins returns a defensive copy of the dynamic HTTP plugin allowlist
// and whether the control plane currently publishes one.
func (snap *ConfigSnapshot) HTTPPlugins() ([]string, bool) {
	return append([]string(nil), snap.httpPlugins...), snap.dynamicPlugins
}

// QuarantinedResources returns the stable bucket and bbolt key for each
// malformed legacy row omitted from this generation. SSL, global rules,
// and the dynamic plugin list are never omitted.
func (snap *ConfigSnapshot) QuarantinedResources() []ConfigQuarantine {
	return append([]ConfigQuarantine(nil), snap.quarantined...)
}

// GetConfigSnapshot returns the current route-build generation, rebuilding it
// once per HTTP route-build resource change.
func GetConfigSnapshot() (*ConfigSnapshot, error) {
	if s == nil {
		return nil, ErrNotFound
	}
	return s.GetConfigSnapshot()
}

// GetConfigSnapshot returns one immutable HTTP route-build generation owned by
// this Store. It never consults the package-level Store singleton.
func (s *Store) GetConfigSnapshot() (*ConfigSnapshot, error) {
	if s == nil {
		return nil, ErrNotFound
	}
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

func (s *Store) getConfigSnapshot() (*ConfigSnapshot, error) {
	return s.GetConfigSnapshot()
}

func (s *Store) buildConfigSnapshot(generation uint64) (*ConfigSnapshot, error) {
	snapshot := &ConfigSnapshot{
		generation:     generation,
		pluginMetadata: map[string]map[string]any{},
		services:       map[string]resource.Service{},
		upstreams:      map[string]resource.Upstream{},
		pluginConfigs:  map[string]resource.PluginConfigRule{},
		ssls:           map[string]resource.SSL{},
	}

	entriesByBucket := make(map[string][]bucketEntry, len(configSnapshotBuckets))
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, bucketName := range configSnapshotBuckets {
			bucket := tx.Bucket([]byte(bucketName))
			if bucket == nil {
				return errBucketNotFound
			}
			entries := make([]bucketEntry, 0)
			if err := bucket.ForEach(func(id, value []byte) error {
				entries = append(entries, bucketEntry{
					id:    string(bytes.Clone(id)),
					value: bytes.Clone(value),
				})
				return nil
			}); err != nil {
				return err
			}
			entriesByBucket[bucketName] = entries
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if hook := s.afterConfigSnapshotBucketRead; hook != nil {
		for _, bucketName := range configSnapshotBuckets {
			hook(bucketName)
		}
	}

	for _, entry := range entriesByBucket["routes"] {
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

	previous := s.configSnapshot.Load()
	for _, entry := range entriesByBucket["global_rules"] {
		rule, err := ParseGlobalRule(entry.value)
		if err != nil {
			lastGood, ok := lastGoodGlobalRule(previous, entry.id)
			if !ok {
				return nil, fmt.Errorf("decode global_rules/%q: %w", entry.id, err)
			}
			snapshot.globalRules = append(snapshot.globalRules, lastGood)
			continue
		}
		if rule.ID == "" {
			rule.ID = entry.id
		}
		snapshot.globalRules = append(snapshot.globalRules, rule)
	}

	for _, entry := range entriesByBucket["plugin_metadata"] {
		id := entry.id
		var metadata map[string]any
		if err := decodePluginMetadata(entry.value, id, &metadata); err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "plugin_metadata",
				ID:     id,
			})
			continue
		}
		if metadata == nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "plugin_metadata",
				ID:     id,
			})
			continue
		}
		snapshot.pluginMetadata[id] = cloneAnyMap(metadata)
	}

	for _, entry := range entriesByBucket["services"] {
		service, err := ParseService(entry.value)
		if err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "services",
				ID:     entry.id,
			})
			continue
		}
		snapshot.services[entry.id] = service
	}
	for _, entry := range entriesByBucket["upstreams"] {
		upstream, err := ParseUpstream(entry.value)
		if err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "upstreams",
				ID:     entry.id,
			})
			continue
		}
		snapshot.upstreams[entry.id] = upstream
	}
	for _, entry := range entriesByBucket["plugin_configs"] {
		config, err := ParsePluginConfigRule(entry.value)
		if err != nil {
			snapshot.quarantined = append(snapshot.quarantined, ConfigQuarantine{
				Bucket: "plugin_configs",
				ID:     entry.id,
			})
			continue
		}
		snapshot.pluginConfigs[entry.id] = config
	}
	for _, entry := range entriesByBucket["ssls"] {
		ssl, err := ParseSSL(entry.value)
		if err != nil {
			lastGood, ok := lastGoodSSL(previous, entry.id)
			if !ok {
				return nil, fmt.Errorf("decode ssls/%q: %w", entry.id, err)
			}
			snapshot.ssls[entry.id] = lastGood
			continue
		}
		snapshot.ssls[entry.id] = ssl
	}

	entries := entriesByBucket["plugins"]
	httpPlugins, dynamicPlugins, err := resolveDynamicHTTPPlugins(entries, previous)
	if err != nil {
		return nil, err
	}
	snapshot.httpPlugins = httpPlugins
	snapshot.dynamicPlugins = dynamicPlugins
	slices.SortFunc(snapshot.quarantined, func(left, right ConfigQuarantine) int {
		if comparison := strings.Compare(left.Bucket, right.Bucket); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.ID, right.ID)
	})
	return snapshot, nil
}

func cloneRoute(route resource.Route) resource.Route {
	route.Uris = append([]string(nil), route.Uris...)
	route.Methods = append([]string(nil), route.Methods...)
	route.Hosts = append([]string(nil), route.Hosts...)
	route.RemoteAddrs = append([]string(nil), route.RemoteAddrs...)
	route.Vars = bytes.Clone(route.Vars)
	route.Script = bytes.Clone(route.Script)
	route.ScriptID = bytes.Clone(route.ScriptID)
	route.Plugins = clonePluginConfigs(route.Plugins)
	route.Labels = cloneStringAnyMap(route.Labels)
	route.Upstream = cloneUpstream(route.Upstream)
	return route
}

func cloneService(service resource.Service) resource.Service {
	service.Plugins = clonePluginConfigs(service.Plugins)
	service.Upstream = cloneUpstream(service.Upstream)
	service.Hosts = append([]string(nil), service.Hosts...)
	return service
}

func cloneConsumer(consumer resource.Consumer) resource.Consumer {
	consumer.Plugins = clonePluginConfigs(consumer.Plugins)
	consumer.Labels = cloneAnyMap(consumer.Labels)
	return consumer
}

func cloneUpstream(upstream resource.Upstream) resource.Upstream {
	upstream.Nodes = append([]resource.Node(nil), upstream.Nodes...)
	upstream.Checks = cloneStringAnyMap(upstream.Checks)
	if upstream.TLS != nil {
		tls := *upstream.TLS
		tls.ClientCertID = cloneAnyValue(tls.ClientCertID)
		upstream.TLS = &tls
	}
	return upstream
}

func cloneGlobalRule(rule resource.GlobalRule) resource.GlobalRule {
	rule.Plugins = clonePluginConfigs(rule.Plugins)
	return rule
}

func cloneStreamRoute(route resource.StreamRoute) resource.StreamRoute {
	route.Plugins = clonePluginConfigs(route.Plugins)
	route.Upstream = cloneUpstream(route.Upstream)
	return route
}

func clonePluginConfigRule(rule resource.PluginConfigRule) resource.PluginConfigRule {
	rule.Plugins = clonePluginConfigs(rule.Plugins)
	return rule
}

func cloneSSL(ssl resource.SSL) resource.SSL {
	ssl.Snis = append([]string(nil), ssl.Snis...)
	if ssl.Labels != nil {
		labels := make(map[string]string, len(ssl.Labels))
		maps.Copy(labels, ssl.Labels)
		ssl.Labels = labels
	}
	return ssl
}

func clonePluginConfigs(source map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	if source == nil {
		return nil
	}
	cloned := make(map[string]resource.PluginConfig, len(source))
	for name, config := range source {
		cloned[name] = cloneAnyValue(config)
	}
	return cloned
}

func cloneAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneAnyValue(value)
	}
	return cloned
}

func cloneStringAnyMap(source map[string]any) map[string]any {
	return cloneAnyMap(source)
}

func cloneAnyValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case []any:
		cloned := make([]any, len(value))
		for index, item := range value {
			cloned[index] = cloneAnyValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), value...)
	case map[string]string:
		cloned := make(map[string]string, len(value))
		maps.Copy(cloned, value)
		return cloned
	default:
		return value
	}
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
