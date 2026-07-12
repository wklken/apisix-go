package etcd

import (
	"context"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/store"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type ConfigClient struct {
	client *clientv3.Client
	prefix string
	// add a channel, receive the etcd change events
	events chan *store.Event

	closeOnce sync.Once
	closeErr  error
}

func NewConfigClient(
	endpoints []string,
	username string,
	password string,
	prefix string,
	events chan *store.Event,
) (*ConfigClient, error) {
	config := clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
		Username:    username,
		Password:    password,
	}

	client, err := clientv3.New(config)
	if err != nil {
		return nil, err
	}

	return &ConfigClient{
		client: client,
		prefix: prefix,
		events: events,
	}, nil
}

func (c *ConfigClient) Watch(contexts ...context.Context) {
	watcher := clientv3.NewWatcher(c.client)
	defer watcher.Close()

	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}

	watchChan := watcher.Watch(ctx, c.prefix, clientv3.WithPrefix())

	for resp := range watchChan {
		// if resp.Err() != nil {
		// 	if errors.Is(resp.Err(), v3rpc.ErrCompacted) {
		// 		logger.Infof("Compaction occurred at revision: %d", resp.CompactRevision)
		// 		watchChan = watcher.Watch(ctx, c.prefix, clientv3.WithPrefix(), clientv3.WithRev(resp.CompactRevision+1))
		// 		continue
		// 	} else {
		// 		// log.Println("Watch canceled due to compaction")
		// 		logger.Errorf("Watch fail due to error: %v", resp.Err())
		// 		// Optionally reset the watch if needed
		// 		watchChan = watcher.Watch(ctx, c.prefix, clientv3.WithPrefix(), clientv3.WithRev(resp.CompactRevision+1))
		// 		continue
		// 	}
		// }

		for _, event := range resp.Events {
			e := store.NewEvent()

			e.Type = store.EventType(event.Type)
			e.Key = event.Kv.Key
			e.Value = event.Kv.Value

			c.events <- e
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := c.client.Get(ctx, c.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	logger.Info("got response")

	for _, kv := range resp.Kvs {
		e := store.NewEvent()
		e.Type = store.EventTypePut
		e.Key = kv.Key
		e.Value = kv.Value

		c.events <- e
	}
	return nil
}
