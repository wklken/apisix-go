package compiler

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestSchemaSetCoversRegisteredFactories(t *testing.T) {
	compiler := newTestCompiler(t)
	wantFactories := 0
	for _, definition := range plugin.Definitions() {
		factory := definition.Factory
		wantFactories++
		entry, ok := compiler.schemas.factories[factory]
		if !ok {
			t.Fatalf("schema set is missing factory %q", factory)
		}
		witness, err := plugin.SchemaWitnessForFactory(factory)
		if err != nil {
			t.Fatal(err)
		}
		if witness.Config != "" && entry.config == nil {
			t.Fatalf("factory %q config schema was not compiled", factory)
		}
		if witness.Metadata != "" && entry.metadata == nil {
			t.Fatalf("factory %q metadata schema was not compiled", factory)
		}
		if witness.HasConsumer != (entry.consumer != nil) {
			t.Fatalf(
				"factory %q consumer schema presence = %v, want %v",
				factory,
				entry.consumer != nil,
				witness.HasConsumer,
			)
		}
		again := compiler.schemas.factories[factory]
		if again.config != entry.config || again.metadata != entry.metadata ||
			again.consumer != entry.consumer {
			t.Fatalf("factory %q schema lookup did not reuse compiled witnesses", factory)
		}
	}
	if got := len(compiler.schemas.factories); got != wantFactories {
		t.Fatalf("schema factory count = %d, want %d", got, wantFactories)
	}
	if compiler.schemas.catalog == nil {
		t.Fatal("schema set secret declaration catalog is nil")
	}
}

func TestRawSchemaAdmissionRejectsInvalidPluginMetadataAndConsumerConfigs(t *testing.T) {
	tests := []struct {
		name        string
		resource    generation.Resource
		wantCode    string
		wantMessage string
		secret      string
	}{
		{
			name: "regular plugin",
			resource: resourceValue(
				"routes", "r1", `{"id":"r1","plugins":{"request-id":{"algorithm":"secret-regular-value"}}}`,
			),
			wantCode: "plugin-schema-invalid", wantMessage: "plugin schema validation failed",
			secret: "secret-regular-value",
		},
		{
			name: "plugin metadata",
			resource: resourceValue(
				"plugin_metadata", "batch-requests", `{"max_pipeline_items":0,"opaque":"secret-metadata-value"}`,
			),
			wantCode: "plugin-metadata-schema-invalid", wantMessage: "plugin metadata schema validation failed",
			secret: "secret-metadata-value",
		},
		{
			name: "consumer plugin",
			resource: resourceValue(
				"consumers",
				"alice",
				`{"username":"alice","plugins":{"jwt-auth":{
					"key":"alice","algorithm":"secret-consumer-value"
				}}}`,
			),
			wantCode: "consumer-schema-invalid", wantMessage: "consumer schema validation failed",
			secret: "secret-consumer-value",
		},
		{
			name: "known non-consumer plugin",
			resource: resourceValue(
				"consumers", "alice", `{"username":"alice","plugins":{"batch-requests":{}}}`,
			),
			wantCode: "consumer-schema-invalid", wantMessage: "consumer schema validation failed",
		},
		{
			name: "unknown metadata factory",
			resource: resourceValue(
				"plugin_metadata", "unknown-secret-factory", `{"opaque":"secret-unknown-value"}`,
			),
			wantCode: "plugin-metadata-schema-invalid", wantMessage: "plugin metadata schema validation failed",
			secret: "secret-unknown-value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiler := newTestCompiler(t)
			input := normalizedSchemaInput(t, test.resource)
			expectedDocument, err := decodeExactDocument(test.resource.Value)
			if err != nil {
				t.Fatal(err)
			}
			beforeRaw := string(input.resources[test.resource.Key].raw)
			result, err := validateContext(context.Background(), input, compiler.schemas)
			if err != nil {
				t.Fatal(err)
			}
			issues := result.issuesForDomain(generation.DomainHTTP)
			if len(issues) != 1 || issues[0].Code != test.wantCode || issues[0].Err.Error() != test.wantMessage {
				t.Fatalf("schema issues = %#v, want %s/%q", issues, test.wantCode, test.wantMessage)
			}
			if test.secret != "" &&
				(strings.Contains(issues[0].Err.Error(), test.secret) ||
					strings.Contains(issues[0].Code, test.secret) ||
					strings.Contains(issues[0].Diagnostic, test.secret)) {
				t.Fatalf("schema issue leaked input %q: %#v", test.secret, issues[0])
			}
			if got := string(input.resources[test.resource.Key].raw); got != beforeRaw {
				t.Fatalf("raw admission mutated resource bytes: got %q, want %q", got, beforeRaw)
			}
			if got := input.resources[test.resource.Key].document; !reflect.DeepEqual(got, expectedDocument) {
				t.Fatalf("raw admission mutated normalized document: got %#v, want %#v", got, expectedDocument)
			}

			desired := mustGenerationSnapshot(t, 41, []generation.Resource{test.resource}, nil)
			set, err := compiler.PreparePublication(
				context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			assertDecision(
				t, set.Domains[generation.DomainHTTP], test.resource.Key,
				generation.DispositionFailClosed, test.wantCode,
			)
		})
	}
}

func TestRawSchemaAdmissionCarriesSafeFieldDiagnostic(t *testing.T) {
	const forbidden = "do-not-log-this-value"
	resource := resourceValue(
		"routes",
		"private-route-id",
		`{"id":"private-route-id","plugins":{"traffic-label":{"rules":[{"match":[["uri","==","/hello"]],"actions":[{"set_headers":{"X-Secret":"`+forbidden+`"},"weight":0.2}]}]}}}`,
	)
	compiler := newTestCompiler(t)
	result, err := validateContext(context.Background(), normalizedSchemaInput(t, resource), compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	issues := result.issuesForDomain(generation.DomainHTTP)
	if len(issues) != 1 {
		t.Fatalf("schema issues = %#v, want one", issues)
	}
	diagnostic := issues[0].Diagnostic
	for _, want := range []string{"traffic-label", "weight", "integer"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("diagnostic = %q, want %q", diagnostic, want)
		}
	}
	for _, forbiddenText := range []string{forbidden, "private-route-id"} {
		if strings.Contains(diagnostic, forbiddenText) {
			t.Fatalf("diagnostic leaked %q: %q", forbiddenText, diagnostic)
		}
	}

	desired := mustGenerationSnapshot(t, 44, []generation.Resource{resource}, nil)
	set, err := compiler.PreparePublication(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	decisions := set.Domains[generation.DomainHTTP].Decisions
	if len(decisions) != 1 || decisions[0].Diagnostic != diagnostic {
		t.Fatalf("publication decisions = %#v, want safe schema diagnostic %q", decisions, diagnostic)
	}
}

func TestRawConsumerSchemaFallsBackToPluginSchema(t *testing.T) {
	for name, test := range map[string]struct {
		config      string
		disposition generation.ResourceDisposition
		code        string
	}{
		"valid non-auth consumer plugin": {
			config:      `{"count":4,"time_window":60}`,
			disposition: generation.DispositionPublished,
			code:        "validated",
		},
		"invalid non-auth consumer plugin": {
			config:      `{"count":0,"time_window":60}`,
			disposition: generation.DispositionFailClosed,
			code:        "consumer-schema-invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertSchemaDecision(
				t,
				resourceValue(
					"consumers", "alice",
					`{"username":"alice","plugins":{"limit-count":`+test.config+`}}`,
				),
				test.disposition,
				test.code,
			)
		})
	}
}

func TestRawConsumerSchemaAdmitsOnlyDeclaredMaterializableEnvelopes(t *testing.T) {
	for _, envelope := range []string{"$ENV://JWT_ALGORITHM", "$secret://vault/team/algorithm", "$encrypted://ciphertext"} {
		t.Run(envelope, func(t *testing.T) {
			assertSchemaDecision(
				t,
				resourceValue(
					"consumers", "alice",
					`{"username":"alice","plugins":{"jwt-auth":{"key":"alice","algorithm":"`+envelope+`"}}}`,
				),
				generation.DispositionPublished,
				"validated",
			)
		})
	}

	for name, config := range map[string]string{
		"plain invalid enum":       `{"key":"alice","algorithm":"plain-invalid"}`,
		"undeclared exp envelope":  `{"key":"alice","exp":"$ENV://EXP"}`,
		"mixed declared unrelated": `{"key":"alice","algorithm":"$ENV://JWT_ALGORITHM","exp":"$ENV://EXP"}`,
		"missing required":         `{"algorithm":"$ENV://JWT_ALGORITHM"}`,
		"empty envelope":           `{"key":"alice","algorithm":"$ENV://"}`,
	} {
		t.Run(name, func(t *testing.T) {
			assertSchemaDecision(
				t,
				resourceValue(
					"consumers", "alice", `{"username":"alice","plugins":{"jwt-auth":`+config+`}}`,
				),
				generation.DispositionFailClosed,
				"consumer-schema-invalid",
			)
		})
	}
}

func TestSchemaAcceptsDeclaredEnvelopeForEverySource(t *testing.T) {
	for _, test := range []struct {
		factory string
		source  capability.SecretDeclarationSource
		field   string
	}{
		{factory: "jwt-auth", source: capability.SecretPluginConfig, field: "private_key"},
		{factory: "azure-functions", source: capability.SecretPluginMetadata, field: "master_apikey"},
		{factory: "key-auth", source: capability.SecretConsumerConfig, field: "key"},
	} {
		t.Run(string(test.source), func(t *testing.T) {
			compiled, err := util.CompileSchema(fmt.Sprintf(
				`{"type":"object","required":[%q],"properties":{%q:{"type":"string","enum":["resolved"]}}}`,
				test.field,
				test.field,
			))
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := capability.NewSecretDeclarationCatalog()
			if err != nil {
				t.Fatal(err)
			}
			accepted, _ := schemaAdmission(
				compiled,
				catalog,
				test.factory,
				test.source,
				map[string]any{test.field: "$ENV://TOKEN"},
			)
			if !accepted {
				t.Fatalf("declared %s envelope was rejected", test.source)
			}
		})
	}
}

func TestRawSchemaAdmissionScopesPluginErrorsAndSkipsPluginsSingleton(t *testing.T) {
	service := resourceValue(
		"services", "shared", `{"id":"shared","plugins":{"request-id":{"algorithm":"invalid"}}}`,
	)
	desired := mustGenerationSnapshot(t, 42, []generation.Resource{service}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(
		t, set.Domains[generation.DomainHTTP], service.Key,
		generation.DispositionFailClosed, "plugin-schema-invalid",
	)
	assertDecision(
		t, set.Domains[generation.DomainStream], service.Key,
		generation.DispositionPublished, "validated",
	)

	assertSchemaDecision(
		t,
		resourceValue("plugins", "plugins", `[{"name":"grpc-transcode","stream":false}]`),
		generation.DispositionPublished,
		"validated",
	)
}

func TestRawSchemaAdmissionPreservesUnknownPluginCode(t *testing.T) {
	assertSchemaDecision(
		t,
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"unknown-plugin":{"secret":"opaque"}}}`),
		generation.DispositionFailClosed,
		"plugin-unsupported",
	)
}

func normalizedSchemaInput(t *testing.T, resources ...generation.Resource) normalizedInput {
	t.Helper()
	input, issues, err := normalize(mustGenerationSnapshot(t, 40, resources, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("normalization issues = %#v", issues)
	}
	return input
}

func assertSchemaDecision(
	t *testing.T,
	resource generation.Resource,
	disposition generation.ResourceDisposition,
	code string,
) {
	t.Helper()
	desired := mustGenerationSnapshot(t, 43, []generation.Resource{
		resource,
		resourceValue("secrets", "vault/team", `{}`),
	}, nil)
	set, err := newTestCompiler(t).PreparePublication(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, set.Domains[generation.DomainHTTP], resource.Key, disposition, code)
}
