package etcd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type (
	watchOpenFunc   func(context.Context, int64) clientv3.WatchChan
	snapshotFunc    func(context.Context) (*clientv3.GetResponse, error)
	healthCheckFunc func(context.Context) error
	getFunc         func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
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
	client *clientv3.Client
	prefix string
	// add a channel, receive the etcd change events
	events chan *store.Event

	closeOnce           sync.Once
	closeErr            error
	requestTimeout      time.Duration
	startupRetry        int
	healthCheck         healthCheckFunc
	healthCheckInterval time.Duration
	watchTimeout        time.Duration
	resyncDelay         time.Duration

	openWatch    watchOpenFunc
	loadSnapshot snapshotFunc
	knownKeys    map[string]struct{}
	// quarantine keeps the latest rejected ModRevision for each full etcd key.
	// It is intentionally internal: metrics expose only the bounded count.
	quarantine   map[string]int64
	lastRevision int64
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

func NewConfigClient(
	endpoints []string,
	username string,
	password string,
	prefix string,
	events chan *store.Event,
) (*ConfigClient, error) {
	return NewConfigClientWithOptions(endpoints, username, password, prefix, events, ClientOptions{})
}

func NewConfigClientWithOptions(
	endpoints []string,
	username string,
	password string,
	prefix string,
	events chan *store.Event,
	options ClientOptions,
) (*ConfigClient, error) {
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

	configClient := &ConfigClient{
		client:              client,
		prefix:              canonicalPrefix,
		events:              events,
		requestTimeout:      options.RequestTimeout,
		startupRetry:        options.StartupRetry,
		healthCheck:         options.HealthCheck,
		healthCheckInterval: options.HealthCheckInterval,
		watchTimeout:        options.WatchTimeout,
		resyncDelay:         options.ResyncDelay,
		knownKeys:           make(map[string]struct{}),
		quarantine:          make(map[string]int64),
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

func newHealthCheck(get getFunc, prefix string) healthCheckFunc {
	return func(ctx context.Context) error {
		_, err := get(ctx, prefix)
		return err
	}
}

func NewTLSConfig(certPath, keyPath, serverName string, verify *bool) (*tls.Config, error) {
	if certPath == "" && keyPath == "" && serverName == "" && verify == nil {
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
		mutation.Value = cloneEtcdValue(event.Kv.Value)
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
	mutations := make([]generation.Mutation, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		if kv == nil {
			return generation.DesiredBatch{}, errors.New("etcd snapshot contains a nil key-value")
		}
		bucket, id, managed := client.managedKey(kv.Key)
		if !managed {
			continue
		}
		mutations = append(mutations, generation.Mutation{
			Type:  generation.MutationPut,
			Key:   generation.ResourceKey{Kind: bucket, ID: id},
			Value: cloneEtcdValue(kv.Value),
		})
	}

	return generation.DesiredBatch{
		Cursor: generation.ProviderCursor{
			Provider: etcdProviderID(response.Header.ClusterId, prefix),
			Revision: strconv.FormatInt(response.Header.Revision, 10),
		},
		ReplaceManaged:  true,
		Mutations:       mutations,
		RequiredDomains: []generation.Domain{generation.DomainHTTP, generation.DomainStream},
	}, nil
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
		batch.Mutations = append(batch.Mutations, mutation)
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

func storeMutationKey(key []byte, bucket, id string) []byte {
	if bucket == "plugins" && id == "plugins" {
		return []byte("/apisix/plugins")
	}
	return key
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
	if ctx == nil {
		ctx = context.Background()
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		c.monitorHealth(monitorCtx)
	}()
	defer func() {
		stopMonitor()
		<-monitorDone
	}()

	revision := c.lastRevision + 1
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
			revision = c.lastRevision + 1
			retry = 0
			continue
		}
		if markUnreachable {
			metrics.RecordEtcdReachable(false)
		}
		for {
			if err := c.recoverSnapshot(ctx); err == nil {
				revision = c.lastRevision + 1
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

func (c *ConfigClient) sendBatch(ctx context.Context, mutations []store.Mutation, options store.BatchOptions) error {
	event := store.NewAcknowledgedBatch(mutations, options)
	select {
	case c.events <- event:
		return event.Wait(ctx)
	case <-ctx.Done():
		store.PutBack(event)
		return ctx.Err()
	}
}

func (c *ConfigClient) sendEvent(ctx context.Context, eventType store.EventType, key, value []byte) error {
	return c.sendBatch(ctx, []store.Mutation{{
		Type:  eventType,
		Key:   key,
		Value: value,
	}}, store.BatchOptions{})
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

func (c *ConfigClient) commitQuarantine(quarantine map[string]int64) {
	c.quarantine = quarantine
	metrics.RecordConfigApplyQuarantine(len(quarantine))
}

type mutationMetadata struct {
	mutation store.Mutation
	key      string
	bucket   string
	id       string
	revision int64
}

type batchApplyResult struct {
	accepted []mutationMetadata
	rejected map[int]struct{}
}

func (c *ConfigClient) applyMutationBatch(
	ctx context.Context,
	candidates []mutationMetadata,
	options store.BatchOptions,
) (batchApplyResult, error) {
	mutations := make([]store.Mutation, len(candidates))
	for index, candidate := range candidates {
		mutations[index] = candidate.mutation
	}
	if err := c.sendBatch(ctx, mutations, options); err == nil {
		return batchApplyResult{accepted: candidates}, nil
	} else {
		var validationErr *store.BatchValidationError
		if !errors.As(err, &validationErr) {
			return batchApplyResult{}, err
		}
		rejected := make(map[int]struct{}, len(validationErr.Rejected))
		preserve := append([]store.ResourceKey(nil), options.Preserve...)
		pruned := make([]mutationMetadata, 0, len(candidates)-len(validationErr.Rejected))
		for _, rejection := range validationErr.Rejected {
			if rejection.Index < 0 || rejection.Index >= len(candidates) {
				return batchApplyResult{}, fmt.Errorf(
					"etcd batch validation returned invalid mutation index %d",
					rejection.Index,
				)
			}
			if _, exists := rejected[rejection.Index]; exists {
				return batchApplyResult{}, fmt.Errorf(
					"etcd batch validation returned duplicate mutation index %d",
					rejection.Index,
				)
			}
			rejected[rejection.Index] = struct{}{}
			candidate := candidates[rejection.Index]
			preserve = append(preserve, store.ResourceKey{Bucket: candidate.bucket, ID: candidate.id})
		}
		for index, candidate := range candidates {
			if _, isRejected := rejected[index]; isRejected {
				continue
			}
			pruned = append(pruned, candidate)
		}
		options.Preserve = preserve
		prunedMutations := make([]store.Mutation, len(pruned))
		for index, candidate := range pruned {
			prunedMutations[index] = candidate.mutation
		}
		if retryErr := c.sendBatch(ctx, prunedMutations, options); retryErr != nil {
			var retryValidationErr *store.BatchValidationError
			if errors.As(retryErr, &retryValidationErr) {
				return batchApplyResult{}, fmt.Errorf("etcd batch validation retry failed: %w", retryErr)
			}
			return batchApplyResult{}, retryErr
		}
		return batchApplyResult{accepted: pruned, rejected: rejected}, nil
	}
}

func (c *ConfigClient) applySnapshot(ctx context.Context, response *clientv3.GetResponse) error {
	if response == nil || response.Header == nil {
		err := errors.New("etcd snapshot is missing a response header")
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
		return err
	}
	nextKeys := make(map[string]struct{}, len(response.Kvs))
	candidates := make([]mutationMetadata, 0, len(response.Kvs))
	for _, kv := range response.Kvs {
		if kv == nil {
			err := errors.New("etcd snapshot contains a nil key-value")
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		bucket, id, ok := c.managedKey(kv.Key)
		if !ok {
			continue
		}
		key := string(kv.Key)
		revision := kv.ModRevision
		if revision <= 0 {
			revision = response.Header.Revision
		}
		nextKeys[key] = struct{}{}
		candidates = append(candidates, mutationMetadata{
			mutation: store.Mutation{
				Type:  store.EventTypePut,
				Key:   storeMutationKey(kv.Key, bucket, id),
				Value: kv.Value,
			},
			key:      key,
			bucket:   bucket,
			id:       id,
			revision: revision,
		})
	}
	nextQuarantine := cloneQuarantine(c.quarantine)
	for key, revision := range nextQuarantine {
		if _, ok := nextKeys[key]; !ok {
			clearQuarantine(nextQuarantine, key, response.Header.Revision)
			if revision > response.Header.Revision {
				nextQuarantine[key] = revision
			}
		}
	}
	result, err := c.applyMutationBatch(ctx, candidates, store.BatchOptions{ReplaceManaged: true})
	if err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
		return err
	}
	for index, candidate := range candidates {
		if _, rejected := result.rejected[index]; rejected {
			recordQuarantine(nextQuarantine, candidate.key, candidate.revision)
			logger.Errorf("quarantine invalid etcd resource key=%q revision=%d", candidate.key, candidate.revision)
			continue
		}
		clearQuarantine(nextQuarantine, candidate.key, candidate.revision)
	}
	for _, candidate := range result.accepted {
		metrics.RecordEtcdModifyIndex(candidate.key, candidate.revision)
	}
	for key := range c.knownKeys {
		if _, ok := nextKeys[key]; ok {
			continue
		}
		if _, _, managed := c.managedKey([]byte(key)); managed {
			metrics.RecordEtcdModifyIndex(key, response.Header.Revision)
		}
	}
	nextRevision := response.Header.Revision
	nextRevision = max(nextRevision, c.lastRevision)
	c.knownKeys = nextKeys
	c.lastRevision = nextRevision
	c.commitQuarantine(nextQuarantine)
	metrics.RecordEtcdAppliedRevision(c.lastRevision)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	return nil
}

func (c *ConfigClient) applyWatchResponse(ctx context.Context, response clientv3.WatchResponse) error {
	nextKeys := make(map[string]struct{}, len(c.knownKeys))
	for key := range c.knownKeys {
		if _, _, managed := c.managedKey([]byte(key)); managed {
			nextKeys[key] = struct{}{}
		}
	}
	candidates := make([]mutationMetadata, 0, len(response.Events))
	for _, watched := range response.Events {
		if watched == nil || watched.Kv == nil {
			err := errors.New("etcd watch response contains a nil event")
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		bucket, id, ok := c.managedKey(watched.Kv.Key)
		if !ok {
			continue
		}
		eventType := store.EventType(watched.Type)
		revision := watched.Kv.ModRevision
		if revision <= 0 {
			revision = response.Header.Revision
		}
		key := string(watched.Kv.Key)
		candidates = append(candidates, mutationMetadata{
			mutation: store.Mutation{
				Type:  eventType,
				Key:   storeMutationKey(watched.Kv.Key, bucket, id),
				Value: watched.Kv.Value,
			},
			key:      key,
			bucket:   bucket,
			id:       id,
			revision: revision,
		})
		if eventType == store.EventTypeDelete {
			delete(nextKeys, key)
		} else {
			nextKeys[key] = struct{}{}
		}
	}
	nextQuarantine := cloneQuarantine(c.quarantine)
	result, err := c.applyMutationBatch(ctx, candidates, store.BatchOptions{})
	if err != nil {
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
		return err
	}
	for index, candidate := range candidates {
		if _, rejected := result.rejected[index]; rejected {
			recordQuarantine(nextQuarantine, candidate.key, candidate.revision)
			logger.Errorf("quarantine invalid etcd resource key=%q revision=%d", candidate.key, candidate.revision)
			continue
		}
		clearQuarantine(nextQuarantine, candidate.key, candidate.revision)
		metrics.RecordEtcdModifyIndex(candidate.key, candidate.revision)
	}
	nextRevision := c.lastRevision
	nextRevision = max(nextRevision, response.Header.Revision)
	for _, candidate := range candidates {
		if candidate.revision > nextRevision {
			nextRevision = candidate.revision
		}
	}
	c.knownKeys = nextKeys
	c.lastRevision = nextRevision
	c.commitQuarantine(nextQuarantine)
	metrics.RecordEtcdAppliedRevision(c.lastRevision)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	return nil
}

func (c *ConfigClient) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.client.Close()
	})
	return c.closeErr
}

func (c *ConfigClient) FetchAll() error {
	return c.FetchAllContext(context.Background())
}

func (c *ConfigClient) FetchAllContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	requestTimeout := c.requestTimeout
	if requestTimeout <= 0 {
		requestTimeout = 5 * time.Second
	}
	var err error
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
