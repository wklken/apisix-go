package request_validation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type requestValidationSecretBroker struct {
	values map[string]string
	calls  []secret.Scope
}

func (broker *requestValidationSecretBroker) ResolveScoped(
	_ context.Context, scope secret.Scope, raw string,
) (string, error) {
	broker.calls = append(broker.calls, scope)
	value, ok := broker.values[raw]
	if !ok {
		return "", fmt.Errorf("request-validation secret is unavailable")
	}
	return value, nil
}

func TestSecretBackedSchemaKeepsPlaintextInsideRequestUse(t *testing.T) {
	const (
		constantRaw = "$ENV://REQUEST_VALIDATION_CONSTANT"
		patternRaw  = "$secret://vault/request-validation/pattern"
		constant    = "generation-private-value"
		pattern     = "^allowed$"
	)
	secrets, scope, broker, closeGeneration := newRequestValidationSecretHarness(t, map[string]string{
		constantRaw: constant,
		patternRaw:  pattern,
	})
	defer closeGeneration()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token": map[string]any{"const": constantRaw},
			"mode":  map[string]any{"pattern": patternRaw},
		},
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	if p.config.bodySchema != nil {
		t.Fatal("secret-backed body schema escaped Value.Use into persistent compiled state")
	}
	if !p.bodySensitive || len(p.bodySecrets) != 2 {
		t.Fatalf("secret-backed body state = sensitive:%t secrets:%d, want true/2", p.bodySensitive, len(p.bodySecrets))
	}
	if len(broker.calls) != 2 {
		t.Fatalf("secret resolution calls = %d, want 2", len(broker.calls))
	}
	for _, call := range broker.calls {
		if call.Field != "body_schema" || call.Plugin != name || call.Source != capability.SecretPluginConfig {
			t.Fatalf("secret scope = %#v, want request-validation body_schema scope", call)
		}
	}

	valid := performRequest(p, http.MethodPost, "/", `{"token":"`+constant+`","mode":"allowed"}`, map[string]string{
		"Content-Type": "application/json",
	})
	if valid.Code != http.StatusNoContent {
		t.Fatalf("valid secret-backed request = %d/%q", valid.Code, valid.Body.String())
	}
	validationErr := p.validateSchema(
		"body_schema", p.config.BodySchema, p.bodySecrets, nil,
		map[string]any{"token": "wrong", "mode": "allowed"},
	)
	if !errors.Is(validationErr, errSensitiveSchemaMismatch) {
		t.Fatalf("secret-backed validation error = %v, want fixed mismatch", validationErr)
	}
	if strings.Contains(validationErr.Error(), constant) || strings.Contains(validationErr.Error(), pattern) {
		t.Fatalf("secret-backed validation error leaked schema material: %v", validationErr)
	}
	var observed []logger.Entry
	stopObserver := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		observed = append(observed, entry)
	})
	defer stopObserver()
	invalid := performRequest(p, http.MethodPost, "/", `{"token":"wrong","mode":"allowed"}`, map[string]string{
		"Content-Type": "application/json",
	})
	message := strings.TrimSpace(invalid.Body.String())
	if invalid.Code != http.StatusBadRequest || message != "request does not match schema" {
		t.Fatalf("invalid secret-backed request = %d/%q", invalid.Code, message)
	}
	if strings.Contains(message, constant) || strings.Contains(message, pattern) {
		t.Fatalf("secret-backed diagnostic leaked schema material: %q", message)
	}
	for _, entry := range observed {
		if strings.Contains(entry.Message, constant) || strings.Contains(entry.Message, pattern) {
			t.Fatalf("secret-backed log leaked schema material: %q", entry.Message)
		}
	}
}

func TestInvalidSecretSchemaCompileErrorDoesNotEscapeUse(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_INVALID_PATTERN"
		plaintext = "[private-invalid-pattern"
	)
	secrets, scope, _, closeGeneration := newRequestValidationSecretHarness(t, map[string]string{raw: plaintext})
	defer closeGeneration()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "string", "pattern": raw,
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("invalid secret schema error = %v, want credential unavailable", err)
	}
	for _, forbidden := range []string{raw, plaintext} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("invalid secret schema error leaked %q: %v", forbidden, err)
		}
	}
	if p.config.bodySchema != nil || len(p.bodySecrets) != 0 {
		t.Fatal("invalid secret schema installed persistent runtime state")
	}
}

func newRequestValidationSecretHarness(
	t *testing.T, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *requestValidationSecretBroker, func()) {
	t.Helper()
	const revision = uint64(307)
	resourceKey := generation.ResourceKey{Kind: "routes", ID: "request-validation-scoped"}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: resourceKey, Value: []byte(`{"plugins":{}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {
				Artifact: generation.GenerationArtifact{
					Domain: generation.DomainHTTP, Revision: revision,
					Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
				},
				Snapshot: snapshot,
				Closure:  []generation.ResourceKey{resourceKey},
				Decisions: []generation.ResourceDecision{{
					Key: resourceKey, Disposition: generation.DispositionPublished,
					Code: "request-validation-scoped-test",
				}},
			},
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &requestValidationSecretBroker{values: values}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision, Domain: generation.DomainHTTP,
		Plugin: name, Resource: resourceKey, Source: capability.SecretPluginConfig,
	}
	return materialization.Secrets(), scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close scoped request-validation generation: %v", err)
		}
	}
}
