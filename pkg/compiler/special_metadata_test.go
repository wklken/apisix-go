package compiler

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	authz_casbin "github.com/wklken/apisix-go/pkg/plugin/authz_casbin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

const specialCasbinModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && r.obj == p.obj && r.act == p.act
`

const specialCasbinPolicy = `
p, alice, /orders/123, GET
p, anonymous, /public, GET
`

func specialCasbinMetadata(model, policy string) string {
	return fmt.Sprintf(`{"model":%q,"policy":%q}`, model, policy)
}

func assertSpecialCasbinFixtureConstructs(t *testing.T, document string) {
	t.Helper()
	view, err := runtime.NewMetadataView(map[string][]byte{
		"authz-casbin": []byte(document),
	})
	if err != nil {
		t.Fatal(err)
	}
	plugin := &authz_casbin.Plugin{}
	plugin.SetDependencies(base.Dependencies{Metadata: view})
	if err := plugin.Init(); err != nil {
		t.Fatalf("authz-casbin Init() error = %v", err)
	}
	config, ok := plugin.Config().(*authz_casbin.Config)
	if !ok {
		t.Fatalf("authz-casbin Config() type = %T", plugin.Config())
	}
	config.Username = "X-User"
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("authz-casbin PostInit() failed to construct the enforcer: %v", err)
	}
}

func TestSpecialMetadataLastGoodAndFailClosedStayInCompiler(t *testing.T) {
	rows := []struct {
		name    string
		factory string
		invalid string
		valid   string
	}{
		{
			name:    "chaitin-waf",
			factory: "chaitin-waf",
			invalid: `{"nodes":[]}`,
			valid:   `{"nodes":[{"host":"127.0.0.1","port":80}],"mode":"monitor"}`,
		},
		{
			name:    "authz-casbin",
			factory: "authz-casbin",
			invalid: `{"model":"model-without-policy"}`,
			valid:   specialCasbinMetadata(specialCasbinModel, specialCasbinPolicy),
		},
		{
			name:    "batch-requests",
			factory: "batch-requests",
			invalid: `{"max_pipeline_items":0}`,
			valid:   `{"max_pipeline_items":8}`,
		},
		{name: "error-log-logger", factory: "error-log-logger", invalid: `{"level":"WARN"}`, valid: `{}`},
		{
			name:    "opentelemetry",
			factory: "opentelemetry",
			invalid: `{"collector":{"request_timeout":"bad"}}`,
			valid:   `{"trace_id_source":"random"}`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			compiler := newTestCompiler(t)
			previousSnapshot := mustGenerationSnapshot(t, 700, []generation.Resource{
				resourceValue("plugin_metadata", row.factory, row.valid),
			}, nil)
			desired := mustGenerationSnapshot(t, 701, []generation.Resource{
				resourceValue("plugin_metadata", row.factory, row.invalid),
			}, nil)
			previous := map[generation.Domain]generation.PublishedGeneration{
				generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, previousSnapshot),
			}
			ticket := ticketForSnapshot(desired, generation.DomainHTTP)
			set, err := compiler.PreparePublication(context.Background(), ticket, desired, previous)
			if err != nil {
				t.Fatal(err)
			}
			candidate := set.Domains[generation.DomainHTTP]
			assertDecision(
				t,
				candidate,
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
				generation.DispositionLastGood,
				"plugin-metadata-schema-invalid",
			)
			got, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory})
			if !found || !bytes.Equal(got, []byte(row.valid)) {
				t.Fatalf("last-good %s bytes = %q/%v, want predecessor %q", row.factory, got, found, row.valid)
			}
			if row.factory == "authz-casbin" {
				assertSpecialCasbinFixtureConstructs(t, row.valid)
			}

			noPredecessorSet, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
			if err != nil {
				t.Fatal(err)
			}
			noPredecessorCandidate := noPredecessorSet.Domains[generation.DomainHTTP]
			assertDecision(
				t,
				noPredecessorCandidate,
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
				generation.DispositionFailClosed,
				"plugin-metadata-schema-invalid",
			)
			if _, found := noPredecessorCandidate.Snapshot.Lookup(
				generation.ResourceKey{Kind: "plugin_metadata", ID: row.factory},
			); found {
				t.Fatal("fail-closed metadata resource leaked into candidate")
			}
		})
	}
}
