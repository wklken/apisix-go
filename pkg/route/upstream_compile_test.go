package route

import (
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestPlanRouteUpstreamPrecedenceAndValidation(t *testing.T) {
	static := &testEffectiveConfig().Config
	upstreams := map[string]resource.Upstream{
		"route-ref": {
			Name:   "route-ref",
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "route-ref", Port: 80, Weight: 1}},
		},
		"service-ref": {
			Name:   "service-ref",
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "service-ref", Port: 80, Weight: 1}},
		},
	}
	route := resource.Route{
		ID: "r1",
		Upstream: resource.Upstream{
			Name:   "inline-route",
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "inline", Port: 80, Weight: 2}},
		},
		UpstreamID: "route-ref",
	}
	service := resource.Service{
		ID: "s1",
		Upstream: resource.Upstream{
			Name:   "inline-service",
			Scheme: "http",
			Nodes:  []resource.Node{{Host: "service", Port: 80, Weight: 1}},
		},
		UpstreamID: "service-ref",
	}
	plan, err := PlanRouteUpstream(route, service, upstreams, nil, static)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Upstream.Name != "inline-route" ||
		plan.Provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "r1"}) {
		t.Fatalf("winner = (%q, %+v), want inline route", plan.Upstream.Name, plan.Provenance)
	}
	if plan.Targets["http://inline:80"] != 2 || plan.ClusterConfig == nil {
		t.Fatalf(
			"targets/cluster = (%+v, %v), want inline target and cluster",
			plan.Targets,
			plan.ClusterConfig,
		)
	}
	for _, test := range []struct {
		name       string
		route      resource.Route
		service    resource.Service
		wantName   string
		provenance plugin.ResourceProvenance
	}{
		{name: "route reference", route: resource.Route{ID: "r1", UpstreamID: "route-ref"}, service: service, wantName: "route-ref", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: "route-ref"}},
		{name: "inline service", route: resource.Route{ID: "r1"}, service: resource.Service{ID: "s1", Upstream: service.Upstream, UpstreamID: "service-ref"}, wantName: "inline-service", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: "s1"}},
		{name: "service reference", route: resource.Route{ID: "r1"}, service: resource.Service{ID: "s1", UpstreamID: "service-ref"}, wantName: "service-ref", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: "service-ref"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := PlanRouteUpstream(test.route, test.service, upstreams, nil, static)
			if err != nil {
				t.Fatal(err)
			}
			if got.Upstream.Name != test.wantName || got.Provenance != test.provenance {
				t.Fatalf(
					"winner = (%q, %+v), want (%q, %+v)",
					got.Upstream.Name,
					got.Provenance,
					test.wantName,
					test.provenance,
				)
			}
		})
	}

	invalid := []resource.Upstream{
		{
			Scheme:   "http",
			PassHost: "rewrite",
			Nodes:    []resource.Node{{Host: "node", Port: 80, Weight: 1}},
		},
		{
			Scheme:   "http",
			PassHost: "invalid",
			Nodes:    []resource.Node{{Host: "node", Port: 80, Weight: 1}},
		},
		{Scheme: "http", Nodes: []resource.Node{{Host: "", Port: 80, Weight: 1}}},
		{Scheme: "http", Nodes: []resource.Node{{Host: "node", Port: 70000, Weight: 1}}},
		{Scheme: "http", Nodes: []resource.Node{{Host: "node", Port: 80, Weight: -1}}},
		{Scheme: "http", Nodes: []resource.Node{{Host: "node", Port: 80, Weight: 0}}},
	}
	for index, upstream := range invalid {
		_, err := PlanRouteUpstream(
			resource.Route{ID: "bad", Upstream: upstream},
			resource.Service{},
			nil,
			nil,
			static,
		)
		if err == nil {
			t.Fatalf("invalid upstream %d error = nil", index)
		}
	}
}

func TestPlanTrafficSplitClusterMatchesCanonicalOrdinaryConfig(t *testing.T) {
	static := &testEffectiveConfig().Config
	routeResource := resource.Route{ID: "r1"}
	ordinary, err := PlanRouteUpstream(routeResource, resource.Service{}, nil, nil, static)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.ClusterConfig == nil {
		t.Fatal("empty ordinary upstream did not prepare an authority-owned transport cluster")
	}

	ordinary, err = PlanRouteUpstream(resource.Route{ID: "r1", Upstream: resource.Upstream{
		Scheme: "http", Nodes: []resource.Node{{Host: "127.0.0.1", Port: 9080, Weight: 1}},
	}}, resource.Service{}, nil, nil, static)
	if err != nil {
		t.Fatal(err)
	}
	traffic, err := PlanTrafficSplitCluster(
		resource.Route{ID: "r1"},
		&traffic_split.Upstream{Scheme: "http"},
		map[string]int{"http://127.0.0.1:9080": 1},
		map[string]int{"http://127.0.0.1:9080": 0},
		nil,
		static,
	)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryKey, err := ordinary.ClusterConfig.Key()
	if err != nil {
		t.Fatal(err)
	}
	trafficKey, err := traffic.Key()
	if err != nil {
		t.Fatal(err)
	}
	if ordinaryKey != trafficKey {
		t.Fatalf("ordinary/traffic cluster key = %x/%x", ordinaryKey, trafficKey)
	}
}

func TestPlanRouteUpstreamClusterIdentity(t *testing.T) {
	static := &testEffectiveConfig().Config
	certA, keyA := routeLeafCertificate(t, "client-a")
	certB, keyB := routeLeafCertificate(t, "client-b")
	base := resource.Upstream{
		Name: "named", Scheme: "https", Nodes: []resource.Node{{Host: "127.0.0.1", Port: 443, Weight: 1, Priority: 2}},
		Checks: map[string]any{"active": map[string]any{"type": "https"}},
		TLS:    &resource.UpstreamTLS{ClientCertID: "ssl-1"},
	}
	plan := func(name string, cert, key string) UpstreamPlan {
		upstream := base
		upstream.Name = name
		result, err := PlanRouteUpstream(
			resource.Route{ID: "r1", Upstream: upstream},
			resource.Service{},
			nil,
			map[string]resource.SSL{
				"ssl-1": {ID: "ssl-1", Cert: cert, Key: key, Status: 1},
			},
			static,
		)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := plan("named", certA, keyA)
	same := plan("named", certA, keyA)
	rotated := plan("named", certB, keyB)
	renamed := plan("renamed", certA, keyA)
	firstKey, _ := first.ClusterConfig.Key()
	sameKey, _ := same.ClusterConfig.Key()
	rotatedKey, _ := rotated.ClusterConfig.Key()
	renamedKey, _ := renamed.ClusterConfig.Key()
	if firstKey != sameKey || firstKey == rotatedKey || firstKey == renamedKey {
		t.Fatalf(
			"cluster keys same/rotated/renamed = %x/%x/%x/%x",
			firstKey,
			sameKey,
			rotatedKey,
			renamedKey,
		)
	}
}

func TestPlanRouteUpstreamEmptyClusterRulesAndInputIsolation(t *testing.T) {
	static := &appconfig.Config{}
	for _, test := range []struct {
		scheme      string
		wantCluster bool
	}{
		{scheme: "http", wantCluster: true}, {scheme: "kafka"}, {scheme: "grpc", wantCluster: true}, {scheme: "grpcs", wantCluster: true},
	} {
		plan, err := PlanRouteUpstream(
			resource.Route{ID: "r1", Upstream: resource.Upstream{Scheme: test.scheme}},
			resource.Service{},
			nil,
			nil,
			static,
		)
		if err != nil {
			t.Fatalf("scheme %s: %v", test.scheme, err)
		}
		if (plan.ClusterConfig != nil) != test.wantCluster {
			t.Fatalf(
				"scheme %s cluster = %v, want %v",
				test.scheme,
				plan.ClusterConfig != nil,
				test.wantCluster,
			)
		}
	}

	checks := map[string]any{"active": map[string]any{"type": "http"}}
	nodes := []resource.Node{{Host: "node", Port: 80, Weight: 1}}
	upstream := resource.Upstream{
		Name:   "owned",
		Scheme: "http",
		Nodes:  nodes,
		Checks: checks,
		TLS:    &resource.UpstreamTLS{Verify: true},
	}
	plan, err := PlanRouteUpstream(
		resource.Route{ID: "r1", Upstream: upstream},
		resource.Service{},
		nil,
		nil,
		static,
	)
	if err != nil {
		t.Fatal(err)
	}
	nodes[0].Host = "mutated"
	checks["active"].(map[string]any)["type"] = "mutated"
	upstream.TLS.Verify = false
	if plan.Upstream.Nodes[0].Host != "node" ||
		plan.Upstream.Checks["active"].(map[string]any)["type"] != "http" ||
		!plan.Upstream.TLS.Verify {
		t.Fatalf("plan observed source mutation: %+v", plan.Upstream)
	}

	cert, key := routeLeafCertificate(t, "owned-client")
	sslInput := map[string]resource.SSL{"ssl-1": {
		ID: "ssl-1", Cert: cert, Key: key, Status: 1,
		Snis: []string{"before.example"}, Labels: map[string]string{"version": "before"},
	}}
	tlsPlan, err := PlanRouteUpstream(resource.Route{ID: "tls", Upstream: resource.Upstream{
		Name: "tls", Scheme: "https", Nodes: []resource.Node{{Host: "node", Port: 443, Weight: 1}},
		TLS: &resource.UpstreamTLS{ClientCertID: "ssl-1"},
	}}, resource.Service{}, nil, sslInput, static)
	if err != nil {
		t.Fatal(err)
	}
	beforeKey, err := tlsPlan.ClusterConfig.Key()
	if err != nil {
		t.Fatal(err)
	}
	mutated := sslInput["ssl-1"]
	mutated.Cert = "mutated"
	mutated.Snis[0] = "after.example"
	mutated.Labels["version"] = "after"
	sslInput["ssl-1"] = mutated
	afterKey, err := tlsPlan.ClusterConfig.Key()
	if err != nil {
		t.Fatal(err)
	}
	if beforeKey != afterKey {
		t.Fatal("planned cluster identity changed after SSL input mutation")
	}
}

func TestPlanRouteUpstreamAppliesStrictTLSPolicyBeforeClusterAcquisition(t *testing.T) {
	static := appconfig.Config{
		SecurityProfile: appconfig.SecurityStrict,
	}
	_, err := PlanRouteUpstream(resource.Route{ID: "strict", Upstream: resource.Upstream{
		Scheme: "https", Nodes: []resource.Node{{Host: "node", Port: 443, Weight: 1}},
		TLS: &resource.UpstreamTLS{Verify: false},
	}}, resource.Service{}, nil, nil, &static)
	if err == nil {
		t.Fatal("strict TLS upstream without verification was not rejected")
	}
}
