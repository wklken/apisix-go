package capability

import (
	"errors"
	"reflect"
	"testing"
)

func TestTransformDeclaredFieldsMatchesCaseInsensitiveLiteralAndDeterministicWildcardMap(t *testing.T) {
	catalog := testWalkCatalog(t, "auth.*.token")
	document := map[string]any{
		"AUTH": map[string]any{
			"secondary": map[string]any{"TOKEN": "second"},
			"Primary":   map[string]any{"Token": "first"},
		},
	}
	var pointers []string
	err := catalog.TransformDeclaredFields(
		"request-id",
		SecretPluginConfig,
		document,
		func(declaration SecretDeclaration, pointer string, value any) (any, error) {
			if declaration.Field != "auth.*.token" {
				t.Fatalf("declaration = %#v", declaration)
			}
			pointers = append(pointers, pointer)
			return "replaced-" + value.(string), nil
		},
	)
	if err != nil {
		t.Fatalf("TransformDeclaredFields() error = %v", err)
	}
	if want := []string{"/AUTH/Primary/Token", "/AUTH/secondary/TOKEN"}; !reflect.DeepEqual(pointers, want) {
		t.Fatalf("pointers = %#v, want %#v", pointers, want)
	}
	auth := document["AUTH"].(map[string]any)
	if auth["Primary"].(map[string]any)["Token"] != "replaced-first" ||
		auth["secondary"].(map[string]any)["TOKEN"] != "replaced-second" {
		t.Fatalf("document = %#v, want deterministic replacements", document)
	}
}

func TestTransformDeclaredFieldsConsumesOneWildcardSliceElement(t *testing.T) {
	catalog := testWalkCatalog(t, "instances.*.auth.token")
	document := map[string]any{
		"instances": []any{
			map[string]any{"auth": map[string]any{"token": "first"}},
			map[string]any{"auth": map[string]any{"token": "second"}},
		},
	}
	var pointers []string
	err := catalog.TransformDeclaredFields(
		"request-id",
		SecretPluginConfig,
		document,
		func(_ SecretDeclaration, pointer string, value any) (any, error) {
			pointers = append(pointers, pointer)
			return value.(string) + "-replaced", nil
		},
	)
	if err != nil {
		t.Fatalf("TransformDeclaredFields() error = %v", err)
	}
	if want := []string{"/instances/0/auth/token", "/instances/1/auth/token"}; !reflect.DeepEqual(pointers, want) {
		t.Fatalf("pointers = %#v, want %#v", pointers, want)
	}
	instances := document["instances"].([]any)
	if instances[0].(map[string]any)["auth"].(map[string]any)["token"] != "first-replaced" ||
		instances[1].(map[string]any)["auth"].(map[string]any)["token"] != "second-replaced" {
		t.Fatalf("document = %#v, want slice replacements", document)
	}
}

func TestTransformDeclaredFieldsRecursesTerminalContainersInDeterministicOrder(t *testing.T) {
	catalog := testWalkCatalog(t, "headers")
	document := map[string]any{
		"headers": map[string]any{
			"z/key": "last",
			"a~key": []any{"first", map[string]any{"nested": "second"}},
			"count": 3,
		},
	}
	var pointers []string
	var values []any
	err := catalog.TransformDeclaredFields(
		"request-id",
		SecretPluginConfig,
		document,
		func(_ SecretDeclaration, pointer string, value any) (any, error) {
			pointers = append(pointers, pointer)
			values = append(values, value)
			if text, ok := value.(string); ok {
				return "wrapped:" + text, nil
			}
			return value, nil
		},
	)
	if err != nil {
		t.Fatalf("TransformDeclaredFields() error = %v", err)
	}
	if want := []string{
		"/headers/a~0key/0",
		"/headers/a~0key/1/nested",
		"/headers/count",
		"/headers/z~1key",
	}; !reflect.DeepEqual(pointers, want) {
		t.Fatalf("pointers = %#v, want %#v", pointers, want)
	}
	if want := []any{"first", "second", 3, "last"}; !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %#v, want %#v", values, want)
	}
	headers := document["headers"].(map[string]any)
	if headers["z/key"] != "wrapped:last" ||
		headers["a~key"].([]any)[1].(map[string]any)["nested"] != "wrapped:second" ||
		headers["count"] != 3 {
		t.Fatalf("document = %#v, want recursive leaf replacements", document)
	}
}

func TestTransformDeclaredFieldsStopsAtCallbackError(t *testing.T) {
	catalog := testWalkCatalog(t, "headers")
	document := map[string]any{
		"headers": map[string]any{"a": "first", "b": "fail", "c": "unvisited"},
	}
	wantErr := errors.New("callback failed")
	var pointers []string
	err := catalog.TransformDeclaredFields(
		"request-id",
		SecretPluginConfig,
		document,
		func(_ SecretDeclaration, pointer string, value any) (any, error) {
			pointers = append(pointers, pointer)
			if value == "fail" {
				return nil, wantErr
			}
			return "replaced", nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("TransformDeclaredFields() error = %v, want callback error", err)
	}
	if want := []string{"/headers/a", "/headers/b"}; !reflect.DeepEqual(pointers, want) {
		t.Fatalf("pointers = %#v, want %#v", pointers, want)
	}
	headers := document["headers"].(map[string]any)
	if headers["a"] != "replaced" || headers["b"] != "fail" || headers["c"] != "unvisited" {
		t.Fatalf("document = %#v, want deterministic short circuit", document)
	}
}

func TestTransformDeclaredFieldsTreatsMissingAndShapeMismatchAsNoOp(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		document any
	}{
		{name: "missing", field: "auth.token", document: map[string]any{"other": "value"}},
		{name: "literal does not consume slice", field: "instances.token", document: map[string]any{
			"instances": []any{map[string]any{"token": "value"}},
		}},
		{name: "wildcard cannot descend scalar", field: "auth.*.token", document: map[string]any{
			"auth": "value",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			catalog := testWalkCatalog(t, tt.field)
			calls := 0
			if err := catalog.TransformDeclaredFields(
				"request-id",
				SecretPluginConfig,
				tt.document,
				func(SecretDeclaration, string, any) (any, error) {
					calls++
					return nil, nil
				},
			); err != nil {
				t.Fatalf("TransformDeclaredFields() error = %v", err)
			}
			if calls != 0 {
				t.Fatalf("callback calls = %d, want 0", calls)
			}
		})
	}
}

func TestTransformDeclaredFieldsDoesNotVisitMapKeys(t *testing.T) {
	catalog := testWalkCatalog(t, "headers")
	document := map[string]any{
		"headers": map[string]any{"$ENV://KEY_NAME": "plain-value"},
	}
	var values []any
	if err := catalog.TransformDeclaredFields(
		"request-id",
		SecretPluginConfig,
		document,
		func(_ SecretDeclaration, _ string, value any) (any, error) {
			values = append(values, value)
			return value, nil
		},
	); err != nil {
		t.Fatalf("TransformDeclaredFields() error = %v", err)
	}
	if want := []any{"plain-value"}; !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %#v, want map values only %#v", values, want)
	}
}

func TestIsMaterializableSecretEnvelopeRequiresSupportedPrefixAndPayload(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "$secret://vault/id/path", want: true},
		{value: "$secret://", want: false},
		{value: "$SECRET://vault/id/path", want: false},
		{value: "$ENV://TOKEN", want: true},
		{value: "$env://TOKEN", want: true},
		{value: "$EnV://TOKEN", want: true},
		{value: "$ENV://", want: false},
		{value: "$encrypted://v2:ciphertext", want: true},
		{value: "$encrypted://", want: false},
		{value: "$ENCRYPTED://ciphertext", want: false},
		{value: "plain", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := IsMaterializableSecretEnvelope(tt.value); got != tt.want {
				t.Fatalf("IsMaterializableSecretEnvelope(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func testWalkCatalog(t *testing.T, field string) *SecretDeclarationCatalog {
	t.Helper()
	return mustDeclarationCatalog(t, []SecretDeclaration{{
		Factory: "request-id",
		Source:  SecretPluginConfig,
		Field:   field,
	}})
}
