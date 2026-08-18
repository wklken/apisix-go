package etcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type (
	watchOpenFunc   func(context.Context, int64) clientv3.WatchChan
	snapshotFunc    func(context.Context) (*clientv3.GetResponse, error)
	healthCheckFunc func(context.Context) error
	getFunc         func(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
)

const defaultHealthCheckInterval = 10 * time.Second

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
		prefix:              prefix,
		events:              events,
		requestTimeout:      options.RequestTimeout,
		startupRetry:        options.StartupRetry,
		healthCheck:         options.HealthCheck,
		healthCheckInterval: options.HealthCheckInterval,
		knownKeys:           make(map[string]struct{}),
		quarantine:          make(map[string]int64),
	}
	configClient.openWatch = func(ctx context.Context, revision int64) clientv3.WatchChan {
		opts := []clientv3.OpOption{clientv3.WithPrefix()}
		if revision > 0 {
			opts = append(opts, clientv3.WithRev(revision))
		}
		return client.Watch(ctx, prefix, opts...)
	}
	configClient.loadSnapshot = func(ctx context.Context) (*clientv3.GetResponse, error) {
		return client.Get(ctx, prefix, clientv3.WithPrefix())
	}
	if configClient.healthCheck == nil {
		configClient.healthCheck = newHealthCheck(client.Get, prefix)
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

func (c *ConfigClient) recoverSnapshot(ctx context.Context) error {
	snapshotCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	response, err := c.loadSnapshot(snapshotCtx)
	if err != nil {
		metrics.RecordEtcdReachable(false)
		return err
	}
	metrics.RecordEtcdReachable(true)
	return c.applySnapshot(ctx, response)
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
		stream := c.openWatch(ctx, revision)
		markUnreachable := true
	watchLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-stream:
				if !ok {
					break watchLoop
				}
				if err := response.Err(); err != nil {
					metrics.RecordEtcdReachable(false)
					logger.Errorf("etcd watch canceled: %v", err)
					break watchLoop
				}
				metrics.RecordEtcdReachable(true)
				if err := c.applyWatchResponse(ctx, response); err != nil {
					if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return
					}
					markUnreachable = false
					logger.Errorf("apply etcd watch response: %v", err)
					break watchLoop
				}
			}
		}
		if ctx.Err() != nil {
			return
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
			if !waitForWatchRetry(ctx, retry) {
				return
			}
			retry++
		}
	}
}

func (c *ConfigClient) sendEvent(ctx context.Context, eventType store.EventType, key, value []byte) error {
	event := store.NewAcknowledgedEvent()
	event.Type = eventType
	event.Key = bytes.Clone(key)
	event.Value = bytes.Clone(value)
	select {
	case c.events <- event:
		return event.Wait(ctx)
	case <-ctx.Done():
		store.PutBack(event)
		return ctx.Err()
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

func resourceValidationError(err error) bool {
	var validationErr *store.ResourceValidationError
	return errors.As(err, &validationErr)
}

func (c *ConfigClient) commitQuarantine(quarantine map[string]int64) {
	c.quarantine = quarantine
	metrics.RecordConfigApplyQuarantine(len(quarantine))
}

func (c *ConfigClient) applySnapshot(ctx context.Context, response *clientv3.GetResponse) error {
	if response == nil || response.Header == nil {
		err := errors.New("etcd snapshot is missing a response header")
		metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
		return err
	}
	nextKeys := make(map[string]struct{}, len(response.Kvs))
	nextQuarantine := cloneQuarantine(c.quarantine)
	for _, kv := range response.Kvs {
		if kv == nil {
			err := errors.New("etcd snapshot contains a nil key-value")
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		nextKeys[string(kv.Key)] = struct{}{}
	}
	keys := make([]string, 0, len(c.knownKeys))
	for key := range c.knownKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := nextKeys[key]; !ok {
			if err := c.sendEvent(ctx, store.EventTypeDelete, []byte(key), nil); err != nil {
				metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
				return err
			}
			clearQuarantine(nextQuarantine, key, response.Header.Revision)
		}
	}
	for _, kv := range response.Kvs {
		revision := kv.ModRevision
		if revision <= 0 {
			revision = response.Header.Revision
		}
		if err := c.sendEvent(ctx, store.EventTypePut, kv.Key, kv.Value); err != nil {
			if resourceValidationError(err) {
				recordQuarantine(nextQuarantine, string(kv.Key), revision)
				logger.Errorf("quarantine invalid etcd resource key=%q revision=%d: %v", kv.Key, revision, err)
				continue
			}
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		clearQuarantine(nextQuarantine, string(kv.Key), revision)
		metrics.RecordEtcdModifyIndex(string(kv.Key), kv.ModRevision)
	}
	for _, key := range keys {
		if _, ok := nextKeys[key]; ok {
			continue
		}
		metrics.RecordEtcdModifyIndex(key, response.Header.Revision)
	}
	c.knownKeys = nextKeys
	c.lastRevision = response.Header.Revision
	c.commitQuarantine(nextQuarantine)
	metrics.RecordEtcdAppliedRevision(c.lastRevision)
	metrics.RecordConfigApplyStageSuccess(metrics.ConfigApplyStageProvider)
	return nil
}

func (c *ConfigClient) applyWatchResponse(ctx context.Context, response clientv3.WatchResponse) error {
	nextKeys := make(map[string]struct{}, len(c.knownKeys))
	for key := range c.knownKeys {
		nextKeys[key] = struct{}{}
	}
	nextRevision := c.lastRevision
	nextQuarantine := cloneQuarantine(c.quarantine)
	for _, watched := range response.Events {
		if watched == nil || watched.Kv == nil {
			err := errors.New("etcd watch response contains a nil event")
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		eventType := store.EventType(watched.Type)
		revision := watched.Kv.ModRevision
		if revision <= 0 {
			revision = response.Header.Revision
		}
		if err := c.sendEvent(ctx, eventType, watched.Kv.Key, watched.Kv.Value); err != nil {
			if resourceValidationError(err) {
				recordQuarantine(nextQuarantine, string(watched.Kv.Key), revision)
				logger.Errorf("quarantine invalid etcd resource key=%q revision=%d: %v", watched.Kv.Key, revision, err)
				if revision > nextRevision {
					nextRevision = revision
				}
				if eventType == store.EventTypeDelete {
					delete(nextKeys, string(watched.Kv.Key))
				} else {
					nextKeys[string(watched.Kv.Key)] = struct{}{}
				}
				continue
			}
			metrics.RecordConfigApplyStageFailure(metrics.ConfigApplyStageProvider)
			return err
		}
		metrics.RecordEtcdModifyIndex(string(watched.Kv.Key), watched.Kv.ModRevision)
		key := string(watched.Kv.Key)
		if eventType == store.EventTypeDelete {
			delete(nextKeys, key)
			clearQuarantine(nextQuarantine, key, revision)
		} else {
			nextKeys[key] = struct{}{}
			clearQuarantine(nextQuarantine, key, revision)
		}
		if watched.Kv.ModRevision > nextRevision {
			nextRevision = watched.Kv.ModRevision
		}
	}
	if response.Header.Revision > nextRevision {
		nextRevision = response.Header.Revision
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
	var err error
	for attempt := 0; attempt <= c.startupRetry; attempt++ {
		loadCtx, loadCancel := context.WithTimeout(context.Background(), c.requestTimeout)
		var resp *clientv3.GetResponse
		resp, err = c.loadSnapshot(loadCtx)
		loadCancel()
		if err != nil {
			metrics.RecordEtcdReachable(false)
			if attempt < c.startupRetry {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		metrics.RecordEtcdReachable(true)
		logger.Info("got response")
		snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), c.requestTimeout)
		defer snapshotCancel()
		return c.applySnapshot(snapshotCtx, resp)
	}
	return err
}
