package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

func TestNewPublishedViewRejectsForgedPublication(t *testing.T) {
	valid := publishedViewGeneration(t, generation.DomainHTTP, []generation.Resource{{
		Key: generation.ResourceKey{
			Kind: "routes",
			ID:   "r1",
		}, Value: []byte(`{"id":"r1","uri":"/"}`),
	}})
	tests := []struct {
		name   string
		mutate func(*generation.PublishedGeneration)
	}{
		{
			name:   "domain",
			mutate: func(p *generation.PublishedGeneration) { p.Artifact.Domain = "udp" },
		},
		{
			name:   "revision",
			mutate: func(p *generation.PublishedGeneration) { p.Artifact.Revision++ },
		},
		{
			name:   "digest",
			mutate: func(p *generation.PublishedGeneration) { p.Artifact.Digest[0]++ },
		},
		{
			name:   "snapshot id",
			mutate: func(p *generation.PublishedGeneration) { p.Artifact.Snapshot += "x" },
		},
		{name: "closure", mutate: func(p *generation.PublishedGeneration) { p.Closure = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forged := clonePublishedGeneration(valid)
			test.mutate(&forged)
			if _, err := NewPublishedView(forged, PublishedViewOptions{}); err == nil {
				t.Fatal("NewPublishedView() error = nil")
			}
		})
	}
}

func TestPublishedViewRawAndPublishedAreImmutable(t *testing.T) {
	value := []byte(`{"id":"r1","uri":"/"}`)
	published := publishedViewGeneration(t, generation.DomainHTTP, []generation.Resource{{
		Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Value: value,
	}})
	view, err := NewPublishedView(published, PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value[0] = 'x'
	published.Closure[0].ID = "input-mutated"
	published.Decisions[0].Code = "input-mutated"
	raw, found := view.Raw("routes", "r1")
	if !found || !bytes.Equal(raw, []byte(`{"id":"r1","uri":"/"}`)) {
		t.Fatalf("Raw() = %q/%v", raw, found)
	}
	raw[0] = 'x'
	second, _ := view.Raw("routes", "r1")
	if second[0] == 'x' {
		t.Fatal("Raw() returned mutable internal bytes")
	}
	copy := view.Published()
	resources := copy.Snapshot.Resources()
	resources[0].Value[0] = 'x'
	copy.Closure[0].ID = "output-mutated"
	copy.Decisions[0].Code = "output-mutated"
	third, _ := view.Raw("routes", "r1")
	if third[0] == 'x' {
		t.Fatal("Published() returned mutable internal bytes")
	}
	internal := view.Published()
	if internal.Closure[0].ID != "r1" || internal.Decisions[0].Code != "view-published" {
		t.Fatalf("Published() exposed mutable closure or decisions: %+v", internal)
	}
}

func TestPublishedViewTypedHTTPResourcesAndConfigSnapshot(t *testing.T) {
	resources := []generation.Resource{
		{
			Key:   generation.ResourceKey{Kind: "routes", ID: "r2"},
			Value: []byte(`{"id":"r2","uri":"/2"}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
			Value: []byte(`{"id":"r1","uri":"/1"}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "services", ID: "svc"},
			Value: []byte(`{"id":"svc","name":"service"}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "upstreams", ID: "up"},
			Value: []byte(`{"type":"roundrobin","nodes":{"127.0.0.1:80":1}}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "consumers", ID: "alice"},
			Value: []byte(`{"username":"alice","plugins":{"key-auth":{"key":"plain"}}}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "consumer_groups", ID: "group"},
			Value: []byte(`{"plugins":{"limit-count":{"count":1}}}`),
		},
		{
			Key: generation.ResourceKey{Kind: "ssls", ID: "ssl"},
			Value: []byte(
				`{"id":"ssl","snis":["example.com"],"client":{"ca":"ca","skip_mtls_uri_regex":["/skip"]}}`,
			),
		},
		{
			Key:   generation.ResourceKey{Kind: "protos", ID: "proto"},
			Value: []byte(`{"id":"proto","content":"syntax = proto3;"}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "plugin_metadata", ID: "custom"},
			Value: []byte(`{"answer":42}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "plugin_configs", ID: "pc"},
			Value: []byte(`{"desc":"rule","plugins":{"limit-count":{"count":1}}}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "global_rules", ID: "g1"},
			Value: []byte(`{"id":"g1","plugins":{"request-id":{}}}`),
		},
		{
			Key:   generation.ResourceKey{Kind: "plugins", ID: "plugins"},
			Value: []byte(`[{"name":"key-auth","stream":false}]`),
		},
	}
	view, err := NewPublishedView(
		publishedViewGeneration(t, generation.DomainHTTP, resources),
		PublishedViewOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := view.Consumer("alice")
	if err != nil || consumer.Username != "alice" {
		t.Fatalf("Consumer() = %+v/%v", consumer, err)
	}
	group, err := view.ConsumerGroup("group")
	if err != nil || group.Plugins["limit-count"] == nil {
		t.Fatalf("ConsumerGroup() = %+v/%v", group, err)
	}
	ssl, err := view.SSL("ssl")
	if err != nil || ssl.ID != "ssl" {
		t.Fatalf("SSL() = %+v/%v", ssl, err)
	}
	ssl.Client.SkipMTLSURIRegex[0] = "mutated"
	sslAgain, _ := view.SSL("ssl")
	if sslAgain.Client.SkipMTLSURIRegex[0] == "mutated" {
		t.Fatal("SSL() returned mutable nested state")
	}
	proto, err := view.Proto("proto")
	if err != nil || proto.Content == "" {
		t.Fatalf("Proto() = %+v/%v", proto, err)
	}
	var metadata map[string]any
	if err := view.PluginMetadata("custom", &metadata); err != nil || metadata["answer"] == nil {
		t.Fatalf("PluginMetadata() = %+v/%v", metadata, err)
	}
	metadataRaw, found := view.PluginMetadataRaw("custom")
	if !found || len(metadataRaw) == 0 {
		t.Fatal("PluginMetadataRaw() missing")
	}

	snapshot, err := view.ConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	routes := snapshot.Routes()
	if len(routes) != 2 || routes[0].ID != "r1" || routes[1].ID != "r2" {
		t.Fatalf("Routes() = %+v, want deterministic ID order", routes)
	}
	if service, err := snapshot.GetService("svc"); err != nil || service.ID != "svc" {
		t.Fatalf("GetService() = %+v/%v", service, err)
	}
	if upstream, err := snapshot.GetUpstream("up"); err != nil || len(upstream.Nodes) != 1 {
		t.Fatalf("GetUpstream() = %+v/%v", upstream, err)
	}
	if rule, err := snapshot.GetPluginConfigRule("pc"); err != nil || rule.Desc != "rule" {
		t.Fatalf("GetPluginConfigRule() = %+v/%v", rule, err)
	}
	snapshotSSL, err := snapshot.GetSSL("ssl")
	if err != nil {
		t.Fatal(err)
	}
	snapshotSSL.Client.SkipMTLSURIRegex[0] = "mutated"
	snapshotSSLAgain, err := snapshot.GetSSL("ssl")
	if err != nil {
		t.Fatal(err)
	}
	if snapshotSSLAgain.Client.SkipMTLSURIRegex[0] == "mutated" {
		t.Fatal("ConfigSnapshot.GetSSL() returned mutable nested state")
	}
	if plugins, ok := snapshot.HTTPPlugins(); !ok || len(plugins) != 1 || plugins[0] != "key-auth" {
		t.Fatalf("HTTPPlugins() = %v/%v", plugins, ok)
	}
}

func TestPublishedViewConfigSnapshotDoesNotSynthesizeEmbeddedIDs(t *testing.T) {
	view, err := NewPublishedView(publishedViewGeneration(t, generation.DomainHTTP, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "routes", ID: "stored-route"}, Value: []byte(`{"uri":"/"}`)},
		{Key: generation.ResourceKey{Kind: "services", ID: "stored-service"}, Value: []byte(`{"name":"service"}`)},
		{
			Key:   generation.ResourceKey{Kind: "global_rules", ID: "stored-rule"},
			Value: []byte(`{"plugins":{"request-id":{}}}`),
		},
		{Key: generation.ResourceKey{Kind: "ssls", ID: "stored-ssl"}, Value: []byte(`{"sni":"example.com"}`)},
	}), PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := view.ConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rules := snapshot.GlobalRules()
	if len(rules) != 1 {
		t.Fatalf("GlobalRules() = %+v, want one rule", rules)
	}
	if rules[0].ID != "" {
		t.Fatalf("GlobalRules()[0].ID = %q, want empty ID preserved for fail-closed validation", rules[0].ID)
	}
	routes := snapshot.Routes()
	if len(routes) != 1 || routes[0].ID != "" {
		t.Errorf("Routes() = %+v, want empty embedded ID preserved", routes)
	}
	service, err := snapshot.GetService("stored-service")
	if err != nil || service.ID != "" {
		t.Errorf("GetService() = %+v/%v, want empty embedded ID preserved", service, err)
	}
	ssl, err := snapshot.GetSSL("stored-ssl")
	if err != nil || ssl.ID != "" {
		t.Errorf("GetSSL() = %+v/%v, want empty embedded ID preserved", ssl, err)
	}
}

func TestPublishedViewPreservesDurableResourceKeyOrder(t *testing.T) {
	httpView, err := NewPublishedView(publishedViewGeneration(t, generation.DomainHTTP, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "routes", ID: "a"}, Value: []byte(`{"id":"z","uri":"/z"}`)},
		{Key: generation.ResourceKey{Kind: "routes", ID: "b"}, Value: []byte(`{"id":"y","uri":"/y"}`)},
		{Key: generation.ResourceKey{Kind: "global_rules", ID: "a"}, Value: []byte(`{"id":"z","plugins":{}}`)},
		{Key: generation.ResourceKey{Kind: "global_rules", ID: "b"}, Value: []byte(`{"id":"y","plugins":{}}`)},
	}), PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := httpView.ConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	routes := snapshot.Routes()
	if len(routes) != 2 || routes[0].ID != "z" || routes[1].ID != "y" {
		t.Errorf("Routes IDs = %v, want durable key a,b order yielding z,y", []string{routes[0].ID, routes[1].ID})
	}
	rules := snapshot.GlobalRules()
	if len(rules) != 2 || rules[0].ID != "z" || rules[1].ID != "y" {
		t.Errorf("GlobalRules IDs = %v, want durable key a,b order yielding z,y", []string{rules[0].ID, rules[1].ID})
	}

	streamView, err := NewPublishedView(publishedViewGeneration(t, generation.DomainStream, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "stream_routes", ID: "a"}, Value: []byte(`{"id":"z","server_port":9001}`)},
		{Key: generation.ResourceKey{Kind: "stream_routes", ID: "b"}, Value: []byte(`{"id":"y","server_port":9002}`)},
	}), PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	streamRoutes, err := streamView.StreamRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if len(streamRoutes) != 2 || streamRoutes[0].ID != "z" || streamRoutes[1].ID != "y" {
		t.Errorf(
			"StreamRoutes IDs = %v, want durable key a,b order yielding z,y",
			[]string{streamRoutes[0].ID, streamRoutes[1].ID},
		)
	}
}

func TestPublishedViewStreamRoutesAreSortedAndCloned(t *testing.T) {
	published := publishedViewGeneration(t, generation.DomainStream, []generation.Resource{
		{
			Key:   generation.ResourceKey{Kind: "stream_routes", ID: "s2"},
			Value: []byte(`{"id":"s2","server_port":9002}`),
		},
		{
			Key: generation.ResourceKey{Kind: "stream_routes", ID: "s1"},
			Value: []byte(
				`{"id":"s1","server_port":9001,"plugins":{"mqtt-proxy":{"protocol_name":"MQTT"}}}`,
			),
		},
	})
	view, err := NewPublishedView(published, PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	routes, err := view.StreamRoutes()
	if err != nil || len(routes) != 2 || routes[0].ID != "s1" || routes[1].ID != "s2" {
		t.Fatalf("StreamRoutes() = %+v/%v", routes, err)
	}
	routes[0].Plugins["mqtt-proxy"].(map[string]any)["protocol_name"] = "mutated"
	again, _ := view.StreamRoutes()
	if again[0].Plugins["mqtt-proxy"].(map[string]any)["protocol_name"] == "mutated" {
		t.Fatal("StreamRoutes() returned mutable plugin config")
	}
}

func TestPublishedViewRejectsHTTPTypedAccessFromStreamDomain(t *testing.T) {
	view, err := NewPublishedView(publishedViewGeneration(t, generation.DomainStream, []generation.Resource{
		{Key: generation.ResourceKey{Kind: "consumers", ID: "alice"}, Value: []byte(`{"username":"alice"}`)},
		{Key: generation.ResourceKey{Kind: "consumer_groups", ID: "group"}, Value: []byte(`{"plugins":{}}`)},
		{Key: generation.ResourceKey{Kind: "ssls", ID: "ssl"}, Value: []byte(`{"id":"ssl"}`)},
		{Key: generation.ResourceKey{Kind: "protos", ID: "proto"}, Value: []byte(`{"id":"proto"}`)},
		{Key: generation.ResourceKey{Kind: "plugin_metadata", ID: "metadata"}, Value: []byte(`{"value":1}`)},
	}), PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, lookup := range map[string]func() error{
		"consumer":       func() error { _, err := view.Consumer("alice"); return err },
		"consumer group": func() error { _, err := view.ConsumerGroup("group"); return err },
		"ssl":            func() error { _, err := view.SSL("ssl"); return err },
		"proto":          func() error { _, err := view.Proto("proto"); return err },
		"plugin metadata": func() error {
			var target map[string]any
			return view.PluginMetadata("metadata", &target)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := lookup(); !errors.Is(err, generation.ErrIntegrity) {
				t.Fatalf("typed stream lookup error = %v, want ErrIntegrity", err)
			}
		})
	}
	if _, found := view.PluginMetadataRaw("metadata"); found {
		t.Fatal("PluginMetadataRaw(stream) found HTTP typed resource")
	}
	if raw, found := view.Raw("consumers", "alice"); !found || len(raw) == 0 {
		t.Fatal("Raw(stream) should remain domain-agnostic")
	}
}

func TestPublishedViewExplicitEncryptionAndJournalIndependence(t *testing.T) {
	service := data_encryption.NewService(true, []string{"qeddd145sfvddff3"})
	pluginSecret, err := service.EncryptForContext("route-secret", "key-auth.key")
	if err != nil {
		t.Fatal(err)
	}
	metadataSecret, err := service.EncryptForContext(
		"metadata-secret",
		"azure-functions.master_apikey",
	)
	if err != nil {
		t.Fatal(err)
	}
	resources := []generation.Resource{
		{Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Value: fmt.Appendf(
			nil, `{"id":"r1","plugins":{"key-auth":{"key":%q}}}`, pluginSecret,
		)},
		{
			Key: generation.ResourceKey{Kind: "plugin_metadata", ID: "azure-functions"},
			Value: fmt.Appendf(
				nil, `{"master_apikey":%q}`, metadataSecret,
			),
		},
	}
	view, err := NewPublishedView(
		publishedViewGeneration(t, generation.DomainHTTP, resources),
		PublishedViewOptions{DataEncryption: service},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := view.ConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Routes()[0].Plugins["key-auth"].(map[string]any)["key"]; got != "route-secret" {
		t.Fatalf("decrypted route key = %v", got)
	}
	var metadata map[string]any
	if err := view.PluginMetadata("azure-functions", &metadata); err != nil ||
		metadata["master_apikey"] != "metadata-secret" {
		t.Fatalf("PluginMetadata() = %+v/%v", metadata, err)
	}

	journal := openTestJournal(t)
	ticket := applyDesiredForPublication(t, journal, "1", generation.DomainHTTP)
	token, err := journal.Stage(
		context.Background(),
		ticket,
		publicationSet(t, ticket, generation.DomainHTTP),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Commit(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	persisted, err := journal.LoadPublished(context.Background(), generation.DomainHTTP)
	if err != nil {
		t.Fatal(err)
	}
	persistedView, err := NewPublishedView(persisted, PublishedViewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found := persistedView.Raw("routes", "r1"); !found {
		t.Fatal("view stopped working after journal Close")
	}
}

func TestPublishedViewConcurrentReads(t *testing.T) {
	view, err := NewPublishedView(
		publishedViewGeneration(t, generation.DomainHTTP, []generation.Resource{
			{
				Key:   generation.ResourceKey{Kind: "consumers", ID: "alice"},
				Value: []byte(`{"username":"alice","plugins":{}}`),
			},
			{
				Key:   generation.ResourceKey{Kind: "routes", ID: "r1"},
				Value: []byte(`{"id":"r1","uri":"/"}`),
			},
		}),
		PublishedViewOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			for range 100 {
				_, _ = view.Raw("routes", "r1")
				_, _ = view.Consumer("alice")
				_, _ = view.ConfigSnapshot()
				_ = view.Published()
			}
		})
	}
	group.Wait()
}

func publishedViewGeneration(
	t *testing.T,
	domain generation.Domain,
	resources []generation.Resource,
) generation.PublishedGeneration {
	t.Helper()
	snapshot := mustSnapshot(t, 1, resources, nil)
	closure := make([]generation.ResourceKey, 0, len(resources))
	decisions := make([]generation.ResourceDecision, 0, len(resources))
	for _, resource := range resources {
		closure = append(closure, resource.Key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: resource.Key, Disposition: generation.DispositionPublished, Code: "view-published",
		})
	}
	return generation.PublishedGeneration{
		Artifact: generation.GenerationArtifact{
			Domain: domain, Revision: 1, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot, Closure: closure, Decisions: decisions,
	}
}

func TestPublishedViewMissingTypedResource(t *testing.T) {
	view, err := NewPublishedView(
		publishedViewGeneration(t, generation.DomainHTTP, nil),
		PublishedViewOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Consumer("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Consumer(missing) error = %v, want ErrNotFound", err)
	}
}
