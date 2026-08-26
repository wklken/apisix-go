package etcd

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestApplySnapshotPreservesPluginIdentityWithCustomPrefix(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	client.prefix = canonicalEtcdPrefix("/custom/root")
	const pluginList = `[{"name":"request-id"}]`
	if err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 40},
		Kvs: []*mvccpb.KeyValue{{
			Key: []byte("/custom/root/plugins"), Value: []byte(pluginList), ModRevision: 40,
		}},
	}); err != nil {
		t.Fatalf("applySnapshot() error = %v", err)
	}
	batches := applier.recordedBatches()
	if len(batches) != 1 || len(batches[0].Mutations) != 1 ||
		batches[0].Mutations[0].Key != (generation.ResourceKey{Kind: "plugins", ID: "plugins"}) ||
		string(batches[0].Mutations[0].Value) != pluginList {
		t.Fatalf("applied plugin batch = %+v", batches)
	}
	if got := client.knownKeys["/custom/root/plugins"]; got != 40 {
		t.Fatalf("known plugin revision = %d, want 40", got)
	}
}

func TestApplyWatchPreservesPluginIdentityWithCustomPrefix(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	client.prefix = canonicalEtcdPrefix("/custom/root")
	const pluginList = `[{"name":"key-auth"}]`
	if err := client.applyWatchResponse(context.Background(), clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 41},
		Events: []*clientv3.Event{{
			Type: mvccpb.PUT,
			Kv: &mvccpb.KeyValue{
				Key: []byte("/custom/root/plugins"), Value: []byte(pluginList), ModRevision: 41,
			},
		}},
	}); err != nil {
		t.Fatalf("applyWatchResponse() error = %v", err)
	}
	batches := applier.recordedBatches()
	if len(batches) != 1 || len(batches[0].Mutations) != 1 ||
		batches[0].Mutations[0].Key != (generation.ResourceKey{Kind: "plugins", ID: "plugins"}) ||
		string(batches[0].Mutations[0].Value) != pluginList {
		t.Fatalf("applied plugin batch = %+v", batches)
	}
	if got := client.knownKeys["/custom/root/plugins"]; got != 41 {
		t.Fatalf("known plugin revision = %d, want 41", got)
	}
}
