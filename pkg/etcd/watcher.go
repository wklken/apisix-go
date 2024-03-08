package etcd

import (
	"context"
	"time"

	"github.com/wklken/apisix-go/pkg/store"
	"go.etcd.io/etcd/clientv3"
)

type ConfigClient struct {
	client *clientv3.Client
	prefix string
	// add a channel, receive the etcd change events
	events chan *store.Event
}

func NewConfigClient(endpoints []string, prefix string, events chan *store.Event) (*ConfigClient, error) {
	config := clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
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

func (c *ConfigClient) Watch() {
	watcher := clientv3.NewWatcher(c.client)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchChan := watcher.Watch(ctx, c.prefix, clientv3.WithPrefix())

	for resp := range watchChan {
		// if resp.Err() != nil {
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

func (c *ConfigClient) FetchAll() error {
	resp, err := c.client.Get(context.Background(), c.prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range resp.Kvs {
		e := store.NewEvent()
		e.Type = store.EventTypePut
		e.Key = kv.Key
		e.Value = kv.Value

		c.events <- e
	}
	return nil
}
