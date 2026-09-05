package etcd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type (
	watchOpenFunc   func(context.Context, int64) clientv3.WatchChan
	snapshotFunc    func(context.Context) (*clientv3.GetResponse, error)
	healthCheckFunc func(context.Context) error
	getFunc         func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
	statusFunc      func(context.Context, string) (*clientv3.StatusResponse, error)
)

const defaultHealthCheckInterval = 10 * time.Second

func canonicalEtcdPrefix(prefix string) string {
	trimmed := strings.Trim(prefix, "/")
	if trimmed == "" {
		return "/"
	}
	return "/" + trimmed + "/"
}

type ConfigClient struct {
	client    *clientv3.Client
	endpoints []string
	prefix    string
	applier   generation.DesiredApplier

	closeOnce           sync.Once
	closeErr            error
	lifecycleMu         sync.Mutex
	lifetimeCtx         context.Context
	cancelLifetime      context.CancelFunc
	activities          sync.WaitGroup
	reporterStarts      sync.WaitGroup
	reporters           map[*ServerInfoReporter]struct{}
	closed              bool
	requestTimeout      time.Duration
	status              statusFunc
	startupRetry        int
	healthCheck         healthCheckFunc
	healthCheckInterval time.Duration
	watchTimeout        time.Duration
	resyncDelay         time.Duration

	openWatch    watchOpenFunc
	loadSnapshot snapshotFunc
	applyMu      sync.Mutex
	knownKeys    map[string]int64
	tombstones   map[string]int64
	// quarantine keeps the latest rejected ModRevision for each full etcd key.
	// It is intentionally internal: metrics expose only the bounded count.
	quarantine         map[string]int64
	lastCursor         generation.ProviderCursor
	lastRevision       int64
	revisions          generation.RevisionSet
	domains            []generation.Domain
	decisions          map[generation.Domain][]generation.ResourceDecision
	collectionVersions map[string]uint64
}

type ClientOptions struct {
	DialTimeout         time.Duration
	RequestTimeout      time.Duration
	StartupRetry        int
	HealthCheck         func(context.Context) error
	HealthCheckInterval time.Duration
	WatchTimeout        time.Duration
	ResyncDelay         time.Duration
	TLS                 *tls.Config
}

func NewConfigClientWithOptions(
	endpoints []string,
	username string,
	password string,
	prefix string,
	applier generation.DesiredApplier,
	options ClientOptions,
) (*ConfigClient, error) {
	if nilDesiredApplier(applier) {
		return nil, errors.New("etcd desired applier is required")
	}
	if options.DialTimeout <= 0 {
		options.DialTimeout = 5 * time.Second
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 5 * time.Second
	}
	if options.StartupRetry < 0 {
		options.StartupRetry = 0
	}
	if options.HealthCheckInterval <= 0 {
		options.HealthCheckInterval = defaultHealthCheckInterval
	}
	canonicalPrefix := canonicalEtcdPrefix(prefix)
	config := clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: options.DialTimeout,
		Username:    username,
		Password:    password,
		TLS:         options.TLS,
	}

	client, err := clientv3.New(config)
	if err != nil {
		return nil, err
	}

	lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
	configClient := &ConfigClient{
		client:              client,
		endpoints:           slices.Clone(endpoints),
		prefix:              canonicalPrefix,
		applier:             applier,
		lifetimeCtx:         lifetimeCtx,
		cancelLifetime:      cancelLifetime,
		reporters:           make(map[*ServerInfoReporter]struct{}),
		requestTimeout:      options.RequestTimeout,
		status:              client.Status,
		startupRetry:        options.StartupRetry,
		healthCheck:         options.HealthCheck,
		healthCheckInterval: options.HealthCheckInterval,
		watchTimeout:        options.WatchTimeout,
		resyncDelay:         options.ResyncDelay,
		knownKeys:           make(map[string]int64),
		tombstones:          make(map[string]int64),
		quarantine:          make(map[string]int64),
		decisions:           make(map[generation.Domain][]generation.ResourceDecision),
		collectionVersions:  make(map[string]uint64),
	}
	configClient.openWatch = func(ctx context.Context, revision int64) clientv3.WatchChan {
		opts := []clientv3.OpOption{clientv3.WithPrefix()}
		if revision > 0 {
			opts = append(opts, clientv3.WithRev(revision))
		}
		return client.Watch(ctx, canonicalPrefix, opts...)
	}
	configClient.loadSnapshot = func(ctx context.Context) (*clientv3.GetResponse, error) {
		return client.Get(ctx, canonicalPrefix, clientv3.WithPrefix())
	}
	if configClient.healthCheck == nil {
		healthPrefix := strings.TrimSuffix(canonicalPrefix, "/")
		if healthPrefix == "" {
			healthPrefix = "/"
		}
		configClient.healthCheck = newHealthCheck(client.Get, healthPrefix)
	}
	return configClient, nil
}

func nilDesiredApplier(applier generation.DesiredApplier) bool {
	if applier == nil {
		return true
	}
	value := reflect.ValueOf(applier)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func newHealthCheck(get getFunc, prefix string) healthCheckFunc {
	return func(ctx context.Context) error {
		_, err := get(ctx, prefix)
		return err
	}
}

func NewTLSConfig(certPath, keyPath, serverName string, verify *bool, trustedCertificate string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" && serverName == "" && verify == nil && trustedCertificate == "" {
		return nil, nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if verify != nil {
		config.InsecureSkipVerify = !*verify
	}
	if certPath != "" || keyPath != "" {
		certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	trustedCertificate = strings.TrimSpace(trustedCertificate)
	if trustedCertificate != "" {
		var roots *x509.CertPool
		var err error
		if trustedCertificate == "system" {
			roots, err = x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system trusted certificates: %w", err)
			}
		} else {
			certificatePEM, readErr := os.ReadFile(trustedCertificate)
			if readErr != nil {
				return nil, fmt.Errorf("read ssl_trusted_certificate %q: %w", trustedCertificate, readErr)
			}
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(certificatePEM) {
				return nil, fmt.Errorf("ssl_trusted_certificate %q contains no certificates", trustedCertificate)
			}
		}
		config.RootCAs = roots
	}
	return config, nil
}

func watchRetryDelay(attempt int) time.Duration {
	delay := 100 * time.Millisecond
	for range min(attempt, 6) {
		delay *= 2
	}
	if delay > 5*time.Second {
		return 5 * time.Second
	}
	return delay
}

func waitForWatchRetry(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(watchRetryDelay(attempt))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *ConfigClient) recoveryDelay(attempt int) time.Duration {
	if c.resyncDelay <= 0 {
		return watchRetryDelay(attempt)
	}
	return c.resyncDelay + time.Duration(rand.Float64()*float64(c.resyncDelay)/2)
}

func (c *ConfigClient) waitForRecoveryRetry(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(c.recoveryDelay(attempt))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func resetWatchIdleTimer(timer *time.Timer, timeout time.Duration) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func stopWatchIdleTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func watchIdleTimerExpired(timer *time.Timer) bool {
	if timer == nil {
		return false
	}
	select {
	case <-timer.C:
		return true
	default:
		return false
	}
}

func (c *ConfigClient) managedKey(key []byte) (string, string, bool) {
	prefix := c.prefix
	if prefix == "" || !strings.HasSuffix(prefix, "/") {
		prefix = canonicalEtcdPrefix(prefix)
	}
	keyString := string(key)
	if !strings.HasPrefix(keyString, prefix) {
		return "", "", false
	}
	relative := strings.TrimPrefix(keyString, prefix)
	if relative == "" {
		return "", "", false
	}
	parts := strings.Split(relative, "/")
	if slices.Contains(parts, "") {
		return "", "", false
	}
	var bucket, id string
	switch {
	case len(parts) == 1 && parts[0] == "plugins":
		bucket, id = "plugins", "plugins"
	case len(parts) == 2:
		bucket, id = parts[0], parts[1]
		if bucket == "secrets" {
			return "", "", false
		}
	case len(parts) == 3 && parts[0] == "secrets":
		bucket, id = "secrets", parts[1]+"/"+parts[2]
	default:
		return "", "", false
	}
	if !generation.IsManagedResourceKind(bucket) {
		return "", "", false
	}
	return bucket, id, true
}

func etcdProviderID(clusterID uint64, prefix string) string {
	canonicalPrefix := canonicalEtcdPrefix(prefix)
	prefixDigest := sha256.Sum256([]byte(canonicalPrefix))
	return fmt.Sprintf("etcd/v1/%016x/%x", clusterID, prefixDigest)
}

func normalizeRequiredDomains(domains []generation.Domain) []generation.Domain {
	hasHTTP := slices.Contains(domains, generation.DomainHTTP)
	hasStream := slices.Contains(domains, generation.DomainStream)
	normalized := make([]generation.Domain, 0, 2)
	if hasHTTP {
		normalized = append(normalized, generation.DomainHTTP)
	}
	if hasStream {
		normalized = append(normalized, generation.DomainStream)
	}
	return normalized
}

func desiredMutationFromEtcdEvent(
	prefix string,
	event *clientv3.Event,
) (generation.Mutation, []generation.Domain, bool, error) {
	client := ConfigClient{prefix: canonicalEtcdPrefix(prefix)}
	bucket, id, managed := client.managedKey(event.Kv.Key)
	if !managed {
		return generation.Mutation{}, nil, false, nil
	}

	mutation := generation.Mutation{Key: generation.ResourceKey{Kind: bucket, ID: id}}
	switch event.Type {
	case mvccpb.PUT:
		mutation.Type = generation.MutationPut
		value, err := canonicalEtcdResourceValue(bucket, id, event.Kv.ModRevision, event.Kv.Value)
		if err != nil {
			return generation.Mutation{}, nil, false, err
		}
		mutation.Value = value
	case mvccpb.DELETE:
		mutation.Type = generation.MutationDelete
	default:
		return generation.Mutation{}, nil, false, fmt.Errorf("unsupported etcd event type %d", event.Type)
	}
	return mutation, generation.DomainsForResourceKind(bucket), true, nil
}

func desiredBatchFromEtcdSnapshot(prefix string, response *clientv3.GetResponse) (generation.DesiredBatch, error) {
	if response == nil || response.Header == nil {
		return generation.DesiredBatch{}, errors.New("etcd snapshot is missing a response header")
	}
	if response.Header.ClusterId == 0 {
		return generation.DesiredBatch{}, errors.New("etcd response requires cluster identity")
	}
	if response.Header.Revision <= 0 {
		return generation.DesiredBatch{}, errors.New("etcd snapshot requires a positive revision")
	}

	client := ConfigClient{prefix: canonicalEtcdPrefix(prefix)}
	provider := etcdProviderID(response.Header.ClusterId, prefix)
	mutations := make([]generation.Mutation, 0, len(response.Kvs))
	collectionVersions := make(map[string]string)
	for _, kv := range response.Kvs {
		if kv == nil {
			return generation.DesiredBatch{}, errors.New("etcd snapshot contains a nil key-value")
		}
		bucket, id, managed := client.managedKey(kv.Key)
		if !managed {
			continue
		}
		if kv.ModRevision <= 0 {
			return generation.DesiredBatch{}, errors.New("etcd managed resource requires a positive modified revision")
		}
		value, err := canonicalEtcdResourceValue(bucket, id, kv.ModRevision, kv.Value)
		if err != nil {
			return generation.DesiredBatch{}, err
		}
		mutations = append(mutations, generation.Mutation{
			Type: generation.MutationPut,
			Key:  generation.ResourceKey{Kind: bucket, ID: id},
			Origin: generation.ResourceOrigin{
				Provider: provider, ResourceKey: string(kv.Key),
				ModifiedIndex: strconv.FormatInt(kv.ModRevision, 10),
			},
			Value: value,
		})
		collectionVersions[bucket] = "1"
	}

	return generation.DesiredBatch{
		Cursor: generation.ProviderCursor{
			Provider: provider,
			Revision: strconv.FormatInt(response.Header.Revision, 10),
		},
		ReplaceManaged:     true,
		Mutations:          mutations,
		CollectionVersions: collectionVersions,
		RequiredDomains:    []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	}, nil
}

func canonicalEtcdResourceValue(bucket, id string, modifiedIndex int64, value []byte) ([]byte, error) {
	if bucket != "consumers" {
		return cloneEtcdValue(value), nil
	}
	if modifiedIndex <= 0 {
		return nil, errors.New("etcd consumer requires a positive modified revision")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(value, &document); err != nil {
		// Invalid resources belong to compiler disposition, not batch translation.
		return cloneEtcdValue(value), nil
	}
	if document == nil {
		return cloneEtcdValue(value), nil
	}
	canonicalID, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("encode etcd consumer id %q: %w", id, err)
	}
	canonicalModifiedIndex, err := json.Marshal(modifiedIndex)
	if err != nil {
		return nil, fmt.Errorf("encode etcd consumer modified revision %q: %w", id, err)
	}
	document["id"] = canonicalID
	document["modifiedIndex"] = canonicalModifiedIndex
	for _, field := range []string{"consumer_name", "auth_conf", "credential_id", "custom_id"} {
		delete(document, field)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode etcd consumer %q: %w", id, err)
	}
	return encoded, nil
}

func desiredBatchFromEtcdWatch(prefix string, response clientv3.WatchResponse) (generation.DesiredBatch, error) {
	if response.Header.ClusterId == 0 {
		return generation.DesiredBatch{}, errors.New("etcd response requires cluster identity")
	}
	if response.Canceled || response.CompactRevision != 0 || response.Created {
		return generation.DesiredBatch{}, errors.New("etcd watch response is not an applicable event batch")
	}

	batch := generation.DesiredBatch{}
	lastEventRevision := int64(0)
	for _, event := range response.Events {
		if event == nil || event.Kv == nil {
			return generation.DesiredBatch{}, errors.New("etcd watch response contains a nil event")
		}
		if event.Kv.ModRevision <= 0 || event.Kv.ModRevision < lastEventRevision {
			return generation.DesiredBatch{}, errors.New("invalid non-monotonic etcd watch event revision")
		}
		lastEventRevision = event.Kv.ModRevision
		mutation, domains, managed, err := desiredMutationFromEtcdEvent(prefix, event)
		if err != nil {
			return generation.DesiredBatch{}, err
		}
		if !managed {
			continue
		}
		if mutation.Type == generation.MutationPut {
			mutation.Origin = generation.ResourceOrigin{
				Provider:      etcdProviderID(response.Header.ClusterId, prefix),
				ResourceKey:   string(event.Kv.Key),
				ModifiedIndex: strconv.FormatInt(event.Kv.ModRevision, 10),
			}
		}
		batch.Mutations = append(batch.Mutations, mutation)
		if batch.CollectionVersions == nil {
			batch.CollectionVersions = make(map[string]string)
		}
		batch.CollectionVersions[mutation.Key.Kind] = "1"
		batch.RequiredDomains = append(batch.RequiredDomains, domains...)
	}

	provider := etcdProviderID(response.Header.ClusterId, prefix)
	if lastEventRevision != 0 {
		if response.Header.Revision < lastEventRevision {
			return generation.DesiredBatch{}, errors.New("etcd watch header precedes its events")
		}
		batch.Cursor = generation.ProviderCursor{
			Provider: provider,
			Revision: strconv.FormatInt(lastEventRevision, 10),
		}
	} else {
		if !response.IsProgressNotify() {
			return generation.DesiredBatch{}, errors.New("empty etcd watch response is not progress")
		}
		batch.Cursor = generation.ProviderCursor{
			Provider: provider,
			Revision: strconv.FormatInt(response.Header.Revision, 10),
		}
	}
	batch.RequiredDomains = normalizeRequiredDomains(batch.RequiredDomains)
	return batch, nil
}

func cloneEtcdValue(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append(make([]byte, 0, len(value)), value...)
}

func (c *ConfigClient) ensureLifetimeLocked() {
	if c.lifetimeCtx != nil {
		return
	}
	c.lifetimeCtx, c.cancelLifetime = context.WithCancel(context.Background())
	if c.reporters == nil {
		c.reporters = make(map[*ServerInfoReporter]struct{})
	}
}

func (c *ConfigClient) beginActivity(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		parent = context.Background()
	}
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		return nil, nil, errors.New("etcd config client is closed")
	}
	c.ensureLifetimeLocked()
	lifetime := c.lifetimeCtx
	c.activities.Add(1)
	c.lifecycleMu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	stopLifetimeCancel := context.AfterFunc(lifetime, cancel)
	var once sync.Once
	done := func() {
		once.Do(func() {
			stopLifetimeCancel()
			cancel()
			c.activities.Done()
		})
	}
	return ctx, done, nil
}

func (c *ConfigClient) nextWatchRevision() int64 {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	return c.lastRevision + 1
}

func (c *ConfigClient) recoverSnapshot(ctx context.Context) error {
	requestTimeout := c.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	loadCtx, loadCancel := context.WithTimeout(ctx, requestTimeout)
	response, err := c.loadSnapshot(loadCtx)
	loadCancel()
	if err != nil {
		metrics.RecordEtcdReachable(false)
		return err
	}
	metrics.RecordEtcdReachable(true)
	applyCtx, applyCancel := context.WithTimeout(ctx, requestTimeout)
	defer applyCancel()
	return c.applySnapshot(applyCtx, response)
}

func (c *ConfigClient) monitorHealth(ctx context.Context) {
	if c.healthCheck == nil {
		return
	}
	interval := c.healthCheckInterval
	if interval <= 0 {
		interval = defaultHealthCheckInterval
	}
	check := func() {
		requestTimeout := c.requestTimeout
		if requestTimeout <= 0 {
			requestTimeout = 5 * time.Second
		}
		checkCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		err := c.healthCheck(checkCtx)
		cancel()
		if ctx.Err() == nil {
			metrics.RecordEtcdReachable(err == nil)
		}
	}
	check()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func (c *ConfigClient) Watch(ctx context.Context) {
	watchCtx, activityDone, err := c.beginActivity(ctx)
	if err != nil {
		return
	}
	defer activityDone()
	ctx = watchCtx
	monitorCtx, stopMonitor := context.WithCancel(watchCtx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		c.monitorHealth(monitorCtx)
	}()
	defer func() {
		stopMonitor()
		<-monitorDone
	}()

	revision := c.nextWatchRevision()
	retry := 0
	for ctx.Err() == nil {
		streamCtx, cancelStream := context.WithCancel(ctx)
		var idleTimer *time.Timer
		var idleTimerC <-chan time.Time
		if c.watchTimeout > 0 {
			idleTimer = time.NewTimer(c.watchTimeout)
			idleTimerC = idleTimer.C
		}
		stream := c.openWatch(streamCtx, revision)
		markUnreachable := true
		idleTimeout := false
	watchLoop:
		for {
			select {
			case <-ctx.Done():
				stopWatchIdleTimer(idleTimer)
				cancelStream()
				return
			case <-idleTimerC:
				idleTimeout = true
				cancelStream()
				break watchLoop
			case response, ok := <-stream:
				if !ok {
					if ctx.Err() == nil && watchIdleTimerExpired(idleTimer) {
						idleTimeout = true
					}
					break watchLoop
				}
				if err := response.Err(); err != nil {
					if ctx.Err() == nil && watchIdleTimerExpired(idleTimer) {
						idleTimeout = true
						break watchLoop
					}
					metrics.RecordEtcdReachable(false)
					logger.Errorf("etcd watch canceled: %v", err)
					break watchLoop
				}
				resetWatchIdleTimer(idleTimer, c.watchTimeout)
				metrics.RecordEtcdReachable(true)
				if err := c.applyWatchResponse(ctx, response); err != nil {
					if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						stopWatchIdleTimer(idleTimer)
						cancelStream()
						return
					}
					markUnreachable = false
					logger.Errorf("apply etcd watch response: %v", err)
					break watchLoop
				}
				resetWatchIdleTimer(idleTimer, c.watchTimeout)
			}
		}
		stopWatchIdleTimer(idleTimer)
		cancelStream()
		if ctx.Err() != nil {
			return
		}
		if idleTimeout {
			revision = c.nextWatchRevision()
			retry = 0
			continue
		}
		if markUnreachable {
			metrics.RecordEtcdReachable(false)
		}
		for {
			if err := c.recoverSnapshot(ctx); err == nil {
				revision = c.nextWatchRevision()
				retry = 0
				break
			} else {
				logger.Errorf("recover etcd watch snapshot: %v", err)
			}
			if !c.waitForRecoveryRetry(ctx, retry) {
				return
			}
			retry++
		}
	}
}

func cloneQuarantine(source map[string]int64) map[string]int64 {
	clone := make(map[string]int64, len(source))
	maps.Copy(clone, source)
	return clone
}

func recordQuarantine(quarantine map[string]int64, key string, revision int64) {
	if previous, ok := quarantine[key]; !ok || revision >= previous {
		quarantine[key] = revision
	}
}

func clearQuarantine(quarantine map[string]int64, key string, revision int64) {
	if previous, ok := quarantine[key]; ok && revision >= previous {
		delete(quarantine, key)
	}
}

type mutationMetadata struct {
	resource generation.ResourceKey
	key      string
	revision int64
	present  bool
}

type etcdProviderCandidate struct {
	batch              generation.DesiredBatch
	knownKeys          map[string]int64
	tombstones         map[string]int64
	authority          map[generation.ResourceKey]mutationMetadata
	modified           map[string]int64
	collectionVersions map[string]uint64
}

func cloneKnownKeys(source map[string]int64) map[string]int64 {
	clone := make(map[string]int64, len(source))
	maps.Copy(clone, source)
	return clone
}

func cloneCollectionVersions(source map[string]uint64) map[string]uint64 {
	clone := make(map[string]uint64, len(source))
	maps.Copy(clone, source)
	return clone
}

func (c *ConfigClient) stageCollectionVersions(batch *generation.DesiredBatch) map[string]uint64 {
	next := cloneCollectionVersions(c.collectionVersions)
	sameCursor := c.lastRevision > 0 && c.lastCursor == batch.Cursor
	for kind := range batch.CollectionVersions {
		version := next[kind]
		if !sameCursor {
			version++
			if version == 0 {
				version = 1
			}
			next[kind] = version
		}
		batch.CollectionVersions[kind] = strconv.FormatUint(version, 10)
	}
	return next
}

func cloneEtcdDecisions(
	source map[generation.Domain][]generation.ResourceDecision,
) map[generation.Domain][]generation.ResourceDecision {
	clone := make(map[generation.Domain][]generation.ResourceDecision, len(source))
	for domain, decisions := range source {
		clone[domain] = slices.Clone(decisions)
	}
	return clone
}

func (c *ConfigClient) resourceMetadata(key string, revision int64, present bool) (mutationMetadata, bool) {
	bucket, id, managed := c.managedKey([]byte(key))
	if !managed {
		return mutationMetadata{}, false
	}
	return mutationMetadata{
		resource: generation.ResourceKey{Kind: bucket, ID: id},
		key:      key, revision: revision, present: present,
	}, true
}

func (c *ConfigClient) fullKeyForResource(resource generation.ResourceKey) (string, bool) {
	if !generation.IsManagedResourceKind(resource.Kind) || resource.ID == "" {
		return "", false
	}
	key := c.prefix + resource.Kind + "/" + resource.ID
	if resource == (generation.ResourceKey{Kind: "plugins", ID: "plugins"}) {
		key = c.prefix + "plugins"
	}
	bucket, id, managed := c.managedKey([]byte(key))
	return key, managed && bucket == resource.Kind && id == resource.ID
}

func (c *ConfigClient) addTombstoneAuthority(
	authority map[generation.ResourceKey]mutationMetadata,
	tombstones map[string]int64,
) {
	for key, revision := range tombstones {
		metadata, managed := c.resourceMetadata(key, revision, false)
		if managed {
			authority[metadata.resource] = metadata
		}
	}
}

func (c *ConfigClient) snapshotCandidate(
	response *clientv3.GetResponse,
) (etcdProviderCandidate, error) {
	batch, err := desiredBatchFromEtcdSnapshot(c.prefix, response)
	if err != nil {
		return etcdProviderCandidate{}, err
	}
	nextCollectionVersions := c.stageCollectionVersions(&batch)
	nextKeys := make(map[string]int64, len(response.Kvs))
	nextTombstones := cloneKnownKeys(c.tombstones)
	authority := make(map[generation.ResourceKey]mutationMetadata, len(response.Kvs)+len(c.knownKeys))
	c.addTombstoneAuthority(authority, nextTombstones)
	modified := make(map[string]int64, len(response.Kvs)+len(c.knownKeys))
	for _, kv := range response.Kvs {
		metadata, managed := c.resourceMetadata(string(kv.Key), kv.ModRevision, true)
		if !managed {
			continue
		}
		if metadata.revision <= 0 {
			metadata.revision = response.Header.Revision
		}
		if previous, exists := authority[metadata.resource]; exists && previous.key != metadata.key {
			return etcdProviderCandidate{}, errors.New("etcd snapshot contains duplicate managed resource identity")
		}
		nextKeys[metadata.key] = metadata.revision
		delete(nextTombstones, metadata.key)
		authority[metadata.resource] = metadata
		modified[metadata.key] = metadata.revision
	}
	for key := range c.knownKeys {
		if _, exists := nextKeys[key]; exists {
			continue
		}
		metadata, managed := c.resourceMetadata(key, response.Header.Revision, false)
		if !managed {
			continue
		}
		authority[metadata.resource] = metadata
		nextTombstones[key] = response.Header.Revision
		modified[key] = response.Header.Revision
	}
	return etcdProviderCandidate{
		batch: batch, knownKeys: nextKeys, tombstones: nextTombstones,
		authority: authority, modified: modified, collectionVersions: nextCollectionVersions,
	}, nil
}

func (c *ConfigClient) watchCandidate(response clientv3.WatchResponse) (etcdProviderCandidate, error) {
	batch, err := desiredBatchFromEtcdWatch(c.prefix, response)
	if err != nil {
		return etcdProviderCandidate{}, err
	}
	nextCollectionVersions := c.stageCollectionVersions(&batch)
	nextKeys := cloneKnownKeys(c.knownKeys)
	nextTombstones := cloneKnownKeys(c.tombstones)
	authority := make(map[generation.ResourceKey]mutationMetadata, len(c.knownKeys)+len(response.Events))
	c.addTombstoneAuthority(authority, nextTombstones)
	for key, revision := range c.knownKeys {
		if metadata, managed := c.resourceMetadata(key, revision, true); managed {
			authority[metadata.resource] = metadata
		}
	}
	modified := make(map[string]int64, len(response.Events))
	for _, watched := range response.Events {
		metadata, managed := c.resourceMetadata(
			string(watched.Kv.Key),
			watched.Kv.ModRevision,
			watched.Type != mvccpb.DELETE,
		)
		if !managed {
			continue
		}
		if previous, exists := authority[metadata.resource]; exists && previous.key != metadata.key {
			return etcdProviderCandidate{}, errors.New("etcd watch contains duplicate managed resource identity")
		}
		authority[metadata.resource] = metadata
		modified[metadata.key] = metadata.revision
		if metadata.present {
			nextKeys[metadata.key] = metadata.revision
			delete(nextTombstones, metadata.key)
		} else {
			delete(nextKeys, metadata.key)
			nextTombstones[metadata.key] = metadata.revision
		}
	}
	return etcdProviderCandidate{
		batch: batch, knownKeys: nextKeys, tombstones: nextTombstones,
		authority: authority, modified: modified, collectionVersions: nextCollectionVersions,
	}, nil
}

func revisionForEtcdDomain(revisions generation.RevisionSet, domain generation.Domain) uint64 {
	switch domain {
	case generation.DomainHTTP:
		return revisions.HTTP
	case generation.DomainStream:
		return revisions.Stream
	default:
		return 0
	}
}

func acknowledgedEtcdDomains(ack generation.Acknowledgement) []generation.Domain {
	domains := make([]generation.Domain, 0, 2)
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		if _, acknowledged := ack.Decisions[domain]; acknowledged {
			domains = append(domains, domain)
		}
	}
	return domains
}

func validEtcdDisposition(disposition generation.ResourceDisposition) bool {
	switch disposition {
	case generation.DispositionPublished, generation.DispositionLastGood,
		generation.DispositionQuarantined, generation.DispositionFailClosed,
		generation.DispositionDeleted:
		return true
	default:
		return false
	}
}

func rejectedEtcdDisposition(disposition generation.ResourceDisposition) bool {
	return disposition == generation.DispositionLastGood ||
		disposition == generation.DispositionQuarantined ||
		disposition == generation.DispositionFailClosed
}

func parseEtcdCursorRevision(cursor generation.ProviderCursor) (int64, error) {
	revision, err := strconv.ParseInt(cursor.Revision, 10, 64)
	if err != nil || revision <= 0 {
		return 0, errors.New("etcd acknowledgement cursor requires a positive numeric revision")
	}
	return revision, nil
}

func (c *ConfigClient) validateAcknowledgement(
	candidate etcdProviderCandidate,
	ack generation.Acknowledgement,
) (map[string]int64, int64, error) {
	batch := candidate.batch
	if ack.Cursor != batch.Cursor {
		return nil, 0, errors.New("etcd acknowledgement cursor mismatch")
	}
	providerRevision, err := parseEtcdCursorRevision(ack.Cursor)
	if err != nil {
		return nil, 0, err
	}
	if c.lastCursor.Provider != "" {
		switch {
		case c.lastCursor.Provider == ack.Cursor.Provider && providerRevision < c.lastRevision:
			return nil, 0, errors.New("etcd acknowledgement cursor regressed")
		case c.lastCursor.Provider != ack.Cursor.Provider && !batch.ReplaceManaged:
			return nil, 0, errors.New("etcd incremental acknowledgement changed provider authority")
		}
	}
	if ack.Revisions.Desired == 0 || ack.Revisions.HTTP > ack.Revisions.Desired ||
		ack.Revisions.Stream > ack.Revisions.Desired {
		return nil, 0, errors.New("etcd acknowledgement revisions are invalid or regressed")
	}
	sameCursor := c.lastCursor == ack.Cursor && c.revisions.Desired != 0
	if c.revisions.Desired != 0 && !sameCursor && ack.Revisions.Desired <= c.revisions.Desired {
		return nil, 0, errors.New("new etcd cursor did not advance desired revision")
	}
	if ack.Revisions.HTTP < c.revisions.HTTP || ack.Revisions.Stream < c.revisions.Stream {
		return nil, 0, errors.New("etcd acknowledgement domain revision regressed")
	}
	if sameCursor && ack.Revisions != c.revisions {
		return nil, 0, errors.New("same etcd cursor returned a different revision set")
	}
	if sameCursor && !reflect.DeepEqual(ack.Decisions, c.decisions) {
		return nil, 0, errors.New("same etcd cursor returned different resource decisions")
	}

	required := make(map[generation.Domain]struct{}, len(batch.RequiredDomains))
	for _, domain := range batch.RequiredDomains {
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return nil, 0, errors.New("etcd desired batch contains an invalid domain")
		}
		if _, duplicate := required[domain]; duplicate {
			return nil, 0, errors.New("etcd desired batch contains a duplicate domain")
		}
		required[domain] = struct{}{}
	}
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		_, domainRequired := required[domain]
		domainRevision := revisionForEtcdDomain(ack.Revisions, domain)
		if domainRequired {
			if domainRevision != ack.Revisions.Desired {
				return nil, 0, errors.New("etcd acknowledged domain does not reference desired revision")
			}
			continue
		}
		if domainRevision != revisionForEtcdDomain(c.revisions, domain) {
			return nil, 0, errors.New("etcd acknowledgement advanced an untouched domain")
		}
	}
	if len(ack.Decisions) != len(required) {
		return nil, 0, errors.New("etcd acknowledgement decision domains mismatch")
	}

	decisions := make(map[generation.Domain]map[generation.ResourceKey]generation.ResourceDecision, len(required))
	for domain, domainDecisions := range ack.Decisions {
		if _, expected := required[domain]; !expected {
			return nil, 0, errors.New("etcd acknowledgement contains an unexpected decision domain")
		}
		indexed := make(map[generation.ResourceKey]generation.ResourceDecision, len(domainDecisions))
		for _, decision := range domainDecisions {
			if decision.Code == "" || !validEtcdDisposition(decision.Disposition) {
				return nil, 0, errors.New("etcd acknowledgement contains an invalid resource decision")
			}
			metadata, acknowledged := candidate.authority[decision.Key]
			if !acknowledged {
				key, managed := c.fullKeyForResource(decision.Key)
				if !batch.ReplaceManaged || !managed ||
					decision.Disposition != generation.DispositionDeleted {
					return nil, 0, errors.New("etcd acknowledgement decision is outside managed provider state")
				}
				metadata = mutationMetadata{
					resource: decision.Key, key: key, revision: providerRevision, present: false,
				}
				candidate.authority[decision.Key] = metadata
				candidate.tombstones[key] = providerRevision
			}
			if !slices.Contains(generation.DomainsForResourceKind(decision.Key.Kind), domain) {
				return nil, 0, errors.New("etcd acknowledgement decision belongs to the wrong domain")
			}
			if _, duplicate := indexed[decision.Key]; duplicate {
				return nil, 0, errors.New("etcd acknowledgement contains duplicate resource decisions")
			}
			if metadata.present && decision.Disposition == generation.DispositionDeleted {
				return nil, 0, errors.New("etcd acknowledgement deleted a present managed resource")
			}
			if !metadata.present && decision.Disposition != generation.DispositionDeleted {
				return nil, 0, errors.New("etcd acknowledgement retained a historical tombstone")
			}
			indexed[decision.Key] = decision
		}
		decisions[domain] = indexed
	}
	for domain := range required {
		if _, found := decisions[domain]; !found {
			return nil, 0, errors.New("etcd acknowledgement omitted a required decision domain")
		}
	}
	for key := range candidate.authority {
		for _, domain := range generation.DomainsForResourceKind(key.Kind) {
			if _, domainRequired := required[domain]; !domainRequired {
				continue
			}
			if _, found := decisions[domain][key]; !found {
				return nil, 0, errors.New("etcd acknowledgement omitted a managed resource decision")
			}
		}
	}

	nextQuarantine := cloneQuarantine(c.quarantine)
	if c.lastCursor.Provider != "" && c.lastCursor.Provider != ack.Cursor.Provider && batch.ReplaceManaged {
		nextQuarantine = make(map[string]int64)
	}
	for key, metadata := range candidate.authority {
		affected := generation.DomainsForResourceKind(key.Kind)
		allAcknowledged := len(affected) > 0
		anyRejected := false
		for _, domain := range affected {
			decision, found := decisions[domain][key]
			if !found {
				allAcknowledged = false
				continue
			}
			if rejectedEtcdDisposition(decision.Disposition) {
				anyRejected = true
			}
		}
		switch {
		case anyRejected:
			recordQuarantine(nextQuarantine, metadata.key, metadata.revision)
		case allAcknowledged:
			clearQuarantine(nextQuarantine, metadata.key, metadata.revision)
		}
	}
	return nextQuarantine, providerRevision, nil
}

func (c *ConfigClient) applyCandidate(ctx context.Context, candidate etcdProviderCandidate) error {
	if nilDesiredApplier(c.applier) {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return errors.New("etcd desired applier is required")
	}
	ack, err := c.applier.Apply(ctx, candidate.batch)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}
	nextQuarantine, providerRevision, err := c.validateAcknowledgement(candidate, ack)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}
	diagnostics, err := generation.DecisionDiagnostics(ack.Decisions)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}
	if err := ctx.Err(); err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}

	c.knownKeys = candidate.knownKeys
	c.tombstones = make(map[string]int64)
	c.quarantine = nextQuarantine
	c.lastCursor = ack.Cursor
	c.lastRevision = providerRevision
	c.revisions = ack.Revisions
	c.domains = acknowledgedEtcdDomains(ack)
	c.decisions = cloneEtcdDecisions(ack.Decisions)
	c.collectionVersions = cloneCollectionVersions(candidate.collectionVersions)
	for key, revision := range candidate.modified {
		metrics.RecordEtcdModifyIndex(key, revision)
	}
	metrics.RecordEtcdAppliedRevision(providerRevision)
	metrics.RecordConfigApplyAcknowledgement(
		ack.Decisions,
		len(nextQuarantine),
	)
	for _, diagnostic := range diagnostics {
		logger.Error(diagnostic)
	}
	return nil
}

func (c *ConfigClient) applySnapshot(ctx context.Context, response *clientv3.GetResponse) error {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	candidate, err := c.snapshotCandidate(response)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}
	return c.applyCandidate(ctx, candidate)
}

func (c *ConfigClient) applyWatchResponse(ctx context.Context, response clientv3.WatchResponse) error {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	candidate, err := c.watchCandidate(response)
	if err != nil {
		metrics.RecordConfigApplyAttemptFailure("etcd", "provider")
		return err
	}
	return c.applyCandidate(ctx, candidate)
}

func (c *ConfigClient) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.lifecycleMu.Lock()
		c.closed = true
		c.ensureLifetimeLocked()
		c.cancelLifetime()
		c.lifecycleMu.Unlock()

		c.reporterStarts.Wait()
		c.lifecycleMu.Lock()
		reporters := make([]*ServerInfoReporter, 0, len(c.reporters))
		for reporter := range c.reporters {
			reporters = append(reporters, reporter)
		}
		c.lifecycleMu.Unlock()
		var reporterErrors []error
		for _, reporter := range reporters {
			if err := reporter.Stop(); err != nil {
				reporterErrors = append(reporterErrors, err)
			}
		}
		c.activities.Wait()
		var clientErr error
		if c.client != nil {
			clientErr = c.client.Close()
		}
		reporterErrors = append(reporterErrors, clientErr)
		c.closeErr = errors.Join(reporterErrors...)
	})
	return c.closeErr
}

func (c *ConfigClient) FetchAll() error {
	return c.FetchAllContext(context.Background())
}

func (c *ConfigClient) FetchAllContext(ctx context.Context) error {
	fetchCtx, activityDone, err := c.beginActivity(ctx)
	if err != nil {
		return err
	}
	defer activityDone()
	ctx = fetchCtx
	requestTimeout := c.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	err = nil
	for attempt := 0; attempt <= c.startupRetry; attempt++ {
		loadCtx, loadCancel := context.WithTimeout(ctx, requestTimeout)
		var resp *clientv3.GetResponse
		resp, err = c.loadSnapshot(loadCtx)
		loadCancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			metrics.RecordEtcdReachable(false)
			if attempt < c.startupRetry {
				if !waitForWatchRetry(ctx, attempt) {
					return ctx.Err()
				}
			}
			continue
		}
		metrics.RecordEtcdReachable(true)
		logger.Info("got response")
		snapshotCtx, snapshotCancel := context.WithTimeout(ctx, requestTimeout)
		applyErr := c.applySnapshot(snapshotCtx, resp)
		snapshotCancel()
		return applyErr
	}
	return err
}
