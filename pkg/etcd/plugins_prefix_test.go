package etcd

import (
	"context"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestApplySnapshotStoresPluginListWithCustomPrefix(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:    canonicalEtcdPrefix("/custom/root"),
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	const pluginList = `[{"name":"request-id"}]`
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{Revision: 40},
		Kvs: []*mvccpb.KeyValue{{
			Key:         []byte("/custom/root/plugins"),
			Value:       []byte(pluginList),
			ModRevision: 40,
		}},
	}); err != nil {
		t.Fatalf("applySnapshot() error = %v", err)
	}
	value, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil || string(value) != pluginList {
		t.Fatalf("stored plugin list = %q, %v; want %s", value, err, pluginList)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("quarantine = %v, want empty", client.quarantine)
	}
}

func TestApplyWatchStoresPluginListWithCustomPrefix(t *testing.T) {
	storage, events := newWatcherStore(t)
	client := &ConfigClient{
		prefix:    canonicalEtcdPrefix("/custom/root"),
		events:    events,
		knownKeys: map[string]struct{}{},
	}
	const pluginList = `[{"name":"key-auth"}]`
	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 41},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key:         []byte("/custom/root/plugins"),
				Value:       []byte(pluginList),
				ModRevision: 41,
			},
		}},
	}); err != nil {
		t.Fatalf("applyWatchResponse() error = %v", err)
	}
	value, err := storage.GetFromBucket("plugins", []byte("plugins"))
	if err != nil || string(value) != pluginList {
		t.Fatalf("stored plugin list = %q, %v; want %s", value, err, pluginList)
	}
	if len(client.quarantine) != 0 {
		t.Fatalf("quarantine = %v, want empty", client.quarantine)
	}
}
