package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestStreamPlanAllowsSharedListenWithDisjointRemoteAddresses(t *testing.T) {
	resources := streamPlannerResources(
		resource.StreamRoute{
			ID:         "private-network",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			RemoteAddr: "10.0.0.0/8",
			Upstream:   testStreamUpstream(1995),
		},
		resource.StreamRoute{
			ID:         "site-network",
			ServerAddr: "127.0.0.1",
			ServerPort: 1883,
			RemoteAddr: "192.168.0.0/16",
			Upstream:   testStreamUpstream(1996),
		},
	)
	_, err := buildStreamPreparationPlan(context.Background(), resources, nil)
	if err != nil {
		t.Fatalf("production plan error = %v, want disjoint remote addresses accepted", err)
	}
}

func TestSharedStreamListenerPreservesDualDomainPreparation(t *testing.T) {
	factory, _ := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 23001, []generation.Resource{
		resourceValue("stream_routes", "private-network", `{
			"id":"private-network",
			"server_addr":"127.0.0.1",
			"server_port":1883,
			"remote_addr":"10.0.0.0/8",
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1995":1}}
		}`),
		resourceValue("stream_routes", "site-network", `{
			"id":"site-network",
			"server_addr":"127.0.0.1",
			"server_port":1883,
			"remote_addr":"192.168.0.0/16",
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1996":1}}
		}`),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatalf("dual-domain prepare: %v", err)
	}
	if prepared.HTTP() == nil || prepared.Stream() == nil {
		t.Fatal("missing prepared domain")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSharedServiceHTTPPluginDoesNotBlockStreamPreparation(t *testing.T) {
	factory, materializer := newWorkerTestFactory(t)
	desired := mustGenerationSnapshot(t, 23002, []generation.Resource{
		resourceValue("services", "shared", `{
			"id":"shared",
			"plugins":{"request-id":{}},
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1995":1}}
		}`),
		resourceValue("routes", "http", `{"id":"http","uri":"/","service_id":"shared"}`),
		resourceValue("stream_routes", "stream", `{
			"id":"stream",
			"service_id":"shared"
		}`),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatalf("dual-domain prepare: %v", err)
	}
	if prepared.HTTP() == nil || prepared.Stream() == nil {
		t.Fatal("missing prepared domain")
	}
	if materializer.registration != nil && materializer.registration.closed != 0 {
		t.Fatal("HTTP registration closed before publication")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
