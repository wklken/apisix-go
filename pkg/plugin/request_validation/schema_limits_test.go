package request_validation

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestSecretBackedSchemaAdmissionRejectsResourceBudgetExcessWithoutPlaintext(t *testing.T) {
	const raw = "$ENV://REQUEST_VALIDATION_SCHEMA_BUDGET"
	tests := []struct {
		name      string
		revision  uint64
		plaintext string
		document  func() map[string]any
	}{
		{
			name:      "encoded-bytes",
			revision:  701,
			plaintext: "private-bytes-" + strings.Repeat("x", requestValidationMaxSchemaBytes),
			document: func() map[string]any {
				return map[string]any{"type": "string", "description": raw}
			},
		},
		{
			name:      "depth",
			revision:  702,
			plaintext: "private-depth",
			document: func() map[string]any {
				nested := map[string]any{"type": "string"}
				for range requestValidationMaxSchemaDepth {
					nested = map[string]any{"not": nested}
				}
				return map[string]any{"description": raw, "allOf": []any{nested}}
			},
		},
		{
			name:      "nodes",
			revision:  703,
			plaintext: "private-nodes",
			document: func() map[string]any {
				return map[string]any{
					"description": raw,
					"examples":    make([]any, requestValidationMaxSchemaNodes),
				}
			},
		},
		{
			name:      "references",
			revision:  704,
			plaintext: "private-references",
			document: func() map[string]any {
				refs := make([]any, requestValidationMaxSchemaReferences+1)
				for index := range refs {
					refs[index] = map[string]any{"$ref": "#"}
				}
				return map[string]any{"description": raw, "allOf": refs}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			secrets, scope, _, closeAttempt := newRequestValidationSecretHarness(
				t, test.revision, map[string]string{raw: test.plaintext},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{BodySchema: test.document()}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			)
			if err == nil {
				err = p.PostInit()
			}
			if !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("budget admission error = %v, want credential unavailable", err)
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), test.plaintext) {
				t.Fatalf("budget admission leaked secret material: %v", err)
			}
			p.Stop()
		})
	}
}

func TestSecretBackedSchemaRejectsExternalReferenceBeforePublication(t *testing.T) {
	const raw = "$ENV://REQUEST_VALIDATION_EXTERNAL_REF"
	externalPath := filepath.Join(t.TempDir(), "external-schema.json")
	if err := os.WriteFile(externalPath, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	externalURL := (&url.URL{Scheme: "file", Path: externalPath}).String()
	secrets, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 705, map[string]string{raw: externalURL},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"description": "external ref must not be loaded", "$ref": raw,
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	err := p.PostInit()
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("external reference admission error = %v, want credential unavailable", err)
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), externalURL) {
		t.Fatalf("external reference error leaked source: %v", err)
	}
	p.Stop()
}

func TestSchemaReferencePolicyRejectsExternalAndRelativeLocations(t *testing.T) {
	for _, reference := range []string{
		"file:///tmp/request-validation.json",
		"http://example.test/request-validation.json",
		"https://example.test/request-validation.json",
		"urn:example:request-validation",
		"request-validation.json",
		"../request-validation.json",
	} {
		t.Run(reference, func(t *testing.T) {
			err := validateRequestValidationSchemaDocument(map[string]any{"$ref": reference}, false)
			if !errors.Is(err, errRequestValidationExternalReference) {
				t.Fatalf("reference policy error = %v, want external-reference rejection", err)
			}
		})
	}
	for _, schemaURL := range []string{
		"file:///tmp/meta-schema.json",
		"https://example.test/meta-schema.json",
		"meta-schema.json",
	} {
		t.Run("schema-"+schemaURL, func(t *testing.T) {
			err := validateRequestValidationSchemaDocument(map[string]any{"$schema": schemaURL}, false)
			if !errors.Is(err, errRequestValidationExternalReference) {
				t.Fatalf("schema policy error = %v, want external-reference rejection", err)
			}
		})
	}
}

func TestSecretBackedSchemaAllowsInDocumentReference(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_INTERNAL_REF_VALUE"
		plaintext = "internal-ref-private-value"
	)
	secrets, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 706, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$defs": map[string]any{
			"token": map[string]any{"type": "string", "const": raw},
		},
		"$ref": "#/$defs/token",
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	response := performRequest(
		p,
		http.MethodPost,
		"/",
		`"`+plaintext+`"`,
		map[string]string{"Content-Type": "application/json"},
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("internal-reference response = %d/%q", response.Code, response.Body.String())
	}
	p.Stop()
}

func TestSchemaReferencePolicyIgnoresReferenceShapedLiteralData(t *testing.T) {
	literal := map[string]any{
		"$ref":    "https://example.test/not-a-schema-reference",
		"$schema": "file:///tmp/not-a-meta-schema.json",
		"nested": map[string]any{
			"$dynamicRef": "urn:not-a-schema-reference",
		},
	}
	document := map[string]any{
		"const": literal,
		"enum":  []any{literal},
	}
	if err := validateRequestValidationSchemaDocument(document, false); err != nil {
		t.Fatalf("literal reference-shaped data rejected: %v", err)
	}
	compiled, err := compileRequestValidationSchema("body_schema", document, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(literal); err != nil {
		t.Fatalf("literal object validation error = %v", err)
	}
}

func TestSecretBackedSchemaAllowsEmbeddedIDReferenceWithoutLoader(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_EMBEDDED_ID_VALUE"
		plaintext = "embedded-id-private-value"
	)
	secrets, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 709, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"$defs": map[string]any{
			"token": map[string]any{
				"$id":  "urn:request-validation:embedded-token",
				"type": "string", "const": raw,
			},
		},
		"$ref": "urn:request-validation:embedded-token",
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	response := performRequest(
		p,
		http.MethodPost,
		"/",
		`"`+plaintext+`"`,
		map[string]string{"Content-Type": "application/json"},
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("embedded-id response = %d/%q", response.Code, response.Body.String())
	}
	p.Stop()
}

func TestNormalizedSchemaMustRemainWithinNodeBudget(t *testing.T) {
	allOf := make([]any, 4095)
	for index := range allOf {
		allOf[index] = map[string]any{"type": "table"}
	}
	document := map[string]any{"allOf": allOf}
	if err := validateRequestValidationSchemaDocument(document, false); err != nil {
		t.Fatalf("raw 8192-node APISIX schema should fit admission budget: %v", err)
	}
	_, err := compileRequestValidationSchema("body_schema", document, false)
	if !errors.Is(err, errRequestValidationSchemaBudget) {
		t.Fatalf("normalized schema error = %v, want node-budget rejection", err)
	}
}
