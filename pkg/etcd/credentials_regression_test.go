package etcd

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestEtcdPreservesNestedConsumerCredentials(t *testing.T) {
	applier := &recordingDesiredApplier{}
	client := newEtcdTestConfigClient(applier)
	err := client.applySnapshot(context.Background(), &clientv3.GetResponse{
		Header: &etcdserverpb.ResponseHeader{ClusterId: 1, Revision: 40},
		Kvs: []*mvccpb.KeyValue{
			{
				Key:         []byte("/apisix/consumers/jack"),
				Value:       []byte(`{"username":"jack"}`),
				ModRevision: 10,
			},
			{
				Key:         []byte("/apisix/consumers/jack/credentials/auth-one"),
				Value:       []byte(`{"id":"jack/credentials/auth-one","plugins":{"key-auth":{"key":"secret-a"}}}`),
				ModRevision: 11,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	batches := applier.recordedBatches()
	if len(batches) != 1 {
		t.Fatalf("applied batches = %d, want 1", len(batches))
	}
	got := map[generation.ResourceKey]generation.Mutation{}
	for _, mutation := range batches[0].Mutations {
		got[mutation.Key] = mutation
	}
	if _, ok := got[generation.ResourceKey{Kind: "consumers", ID: "jack"}]; !ok {
		t.Fatalf("consumer jack missing from snapshot batch: %+v", batches[0].Mutations)
	}
	credential := generation.ResourceKey{Kind: "consumers", ID: "jack/credentials/auth-one"}
	if _, ok := got[credential]; !ok {
		t.Fatal("nested credential was dropped from consumer resources")
	}
	if _, _, managed := client.managedKey([]byte("/apisix/consumers/jack/credentials/auth-one")); !managed {
		t.Fatal("managedKey dropped credential path")
	}
}

func TestEtcdCredentialDeleteDoesNotDeleteParent(t *testing.T) {
	mutation, domains, managed, err := desiredMutationFromEtcdEvent(
		"/apisix",
		&clientv3.Event{
			Type: mvccpb.DELETE,
			Kv:   &mvccpb.KeyValue{Key: []byte("/apisix/consumers/jack/credentials/auth-one"), ModRevision: 12},
		},
	)
	if err != nil || !managed || mutation.Type != generation.MutationDelete ||
		mutation.Key != (generation.ResourceKey{Kind: "consumers", ID: "jack/credentials/auth-one"}) ||
		len(domains) != 1 ||
		domains[0] != generation.DomainHTTP {
		t.Fatalf("delete=%+v domains=%v managed=%v err=%v", mutation, domains, managed, err)
	}
}
