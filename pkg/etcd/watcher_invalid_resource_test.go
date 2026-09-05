package etcd

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestInvalidConsumerDoesNotBlockRouteDeletion(t *testing.T) {
	applier := &recordingDesiredApplier{}
	coordinator := generation.NewCoordinator(&etcdCoordinatorTestEngine{})
	applier.apply = coordinator.Apply
	c := newEtcdTestConfigClient(applier)
	defer func() { _ = c.Close() }()
	initial := &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 1},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/routes/revoked"), Value: []byte(`{"id":"revoked","uri":"/revoked"}`), ModRevision: 1},
		},
	}
	if err := c.applySnapshot(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	response := clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 11},
		Events: []*clientv3.Event{
			{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("/apisix/consumers/broken"),
					Value:       []byte(`"not-an-object"`),
					ModRevision: 10,
				},
			},
			{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte("/apisix/routes/revoked"), ModRevision: 11}},
		},
	}
	err := c.applyWatchResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if _, retained := c.knownKeys["/apisix/routes/revoked"]; retained {
		t.Fatal("deleted route remains acknowledged")
	}
	snapshot := &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 12},
		Kvs: []*mvccpb.KeyValue{
			{Key: []byte("/apisix/consumers/broken"), Value: []byte(`"not-an-object"`), ModRevision: 10},
		},
	}
	err = c.applySnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(applier.recordedBatches()) != 3 || c.lastRevision != 12 {
		t.Fatalf(
			"malformed consumer prevents deletion/recovery reaching coordinator: acknowledged revision=%d, retained route=%d",
			c.lastRevision,
			c.knownKeys["/apisix/routes/revoked"],
		)
	}
}
