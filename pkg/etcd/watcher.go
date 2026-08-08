package etcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type (
	watchOpenFunc func(context.Context, int64) clientv3.WatchChan
	snapshotFunc  func(context.Context) (*clientv3.GetResponse, error)
)

type ConfigClient struct {
	client *clientv3.Client
	prefix string
	// add a channel, receive the etcd change events
	events chan *store.Event

	closeOnce      sync.Once
	closeErr       error
	requestTimeout time.Duration
	startupRetry   int

	openWatch    watchOpenFunc
	loadSnapshot snapshotFunc
	knownKeys    map[string]struct{}
	lastRevision int64
}

type ClientOptions struct {
	DialTimeout    time.Duration
	RequestTimeout time.Duration
	StartupRetry   int
	TLS            *tls.Config
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
		client:         client,
		prefix:         prefix,
		events:         events,
		requestTimeout: options.RequestTimeout,
		startupRetry:   options.StartupRetry,
		knownKeys:      make(map[string]struct{}),
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
	return configClient, nil
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
		return err
	}
	return c.applySnapshot(ctx, response)
}

func (c *ConfigClient) Watch(ctx context.Context) {
	revision := c.lastRevision + 1
	retry := 0
	for ctx.Err() == nil {
		stream := c.openWatch(ctx, revision)
		for response := range stream {
			if err := response.Err(); err != nil {
				logger.Errorf("etcd watch canceled: %v", err)
				break
			}
			if !c.applyWatchResponse(ctx, response) {
				return
			}
		}
		if ctx.Err() != nil {
			return
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

func (c *ConfigClient) sendEvent(ctx context.Context, eventType store.EventType, key, value []byte) bool {
	event := store.NewEvent()
	event.Type = eventType
	event.Key = bytes.Clone(key)
	event.Value = bytes.Clone(value)
	select {
	case c.events <- event:
		return true
	case <-ctx.Done():
		store.PutBack(event)
		return false
	}
}

func (c *ConfigClient) applySnapshot(ctx context.Context, response *clientv3.GetResponse) error {
	if response == nil || response.Header == nil {
		return errors.New("etcd snapshot is missing a response header")
	}
	nextKeys := make(map[string]struct{}, len(response.Kvs))
	for _, kv := range response.Kvs {
		nextKeys[string(kv.Key)] = struct{}{}
	}
	for key := range c.knownKeys {
		if _, ok := nextKeys[key]; !ok && !c.sendEvent(ctx, store.EventTypeDelete, []byte(key), nil) {
			return ctx.Err()
		}
	}
	for _, kv := range response.Kvs {
		if !c.sendEvent(ctx, store.EventTypePut, kv.Key, kv.Value) {
			return ctx.Err()
		}
	}
	c.knownKeys = nextKeys
	c.lastRevision = response.Header.Revision
	return nil
}

func (c *ConfigClient) applyWatchResponse(ctx context.Context, response clientv3.WatchResponse) bool {
	for _, watched := range response.Events {
		eventType := store.EventType(watched.Type)
		if !c.sendEvent(ctx, eventType, watched.Kv.Key, watched.Kv.Value) {
			return false
		}
		key := string(watched.Kv.Key)
		if eventType == store.EventTypeDelete {
			delete(c.knownKeys, key)
		} else {
			c.knownKeys[key] = struct{}{}
		}
		if watched.Kv.ModRevision > c.lastRevision {
			c.lastRevision = watched.Kv.ModRevision
		}
	}
	if response.Header.Revision > c.lastRevision {
		c.lastRevision = response.Header.Revision
	}
	return true
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
			if attempt < c.startupRetry {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		logger.Info("got response")
		snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), c.requestTimeout)
		defer snapshotCancel()
		return c.applySnapshot(snapshotCtx, resp)
	}
	return err
}
