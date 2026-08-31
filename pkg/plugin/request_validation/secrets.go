package request_validation

import (
	"context"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

type schemaPathStep struct {
	key     string
	index   int
	isIndex bool
}

type schemaSecret struct {
	path  []schemaPathStep
	value secret.Value
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	headerDocument, headerSecrets, err := materializeSchemaDocument(
		ctx, access, "header_schema", p.config.HeaderSchema,
	)
	if err != nil {
		return err
	}
	bodyDocument, bodySecrets, err := materializeSchemaDocument(
		ctx, access, "body_schema", p.config.BodySchema,
	)
	if err != nil {
		return err
	}

	if err := validateMaterializedSchema("header_schema", headerDocument, headerSecrets); err != nil {
		return secret.ErrCredentialUnavailable
	}
	if err := validateMaterializedSchema("body_schema", bodyDocument, bodySecrets); err != nil {
		return secret.ErrCredentialUnavailable
	}

	p.config.HeaderSchema = headerDocument
	p.config.BodySchema = bodyDocument
	if len(headerSecrets) > 0 {
		p.headerSensitive = true
		p.headerSecrets = headerSecrets
	}
	if len(bodySecrets) > 0 {
		p.bodySensitive = true
		p.bodySecrets = bodySecrets
	}
	return nil
}

func materializeSchemaDocument(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	document map[string]any,
) (map[string]any, []schemaSecret, error) {
	if document == nil {
		return nil, nil, nil
	}
	value, entries, err := materializeSchemaValue(ctx, access, field, document, nil)
	if err != nil {
		return nil, nil, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, nil, secret.ErrCredentialUnavailable
	}
	return result, entries, nil
}

func materializeSchemaValue(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	value any,
	path []schemaPathStep,
) (any, []schemaSecret, error) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var entries []schemaSecret
		for _, key := range keys {
			child, childEntries, err := materializeSchemaValue(
				ctx, access, field, typed[key], appendSchemaPath(path, schemaPathStep{key: key}),
			)
			if err != nil {
				return nil, nil, err
			}
			result[key] = child
			entries = append(entries, childEntries...)
		}
		return result, entries, nil
	case []any:
		result := make([]any, len(typed))
		var entries []schemaSecret
		for index, childValue := range typed {
			child, childEntries, err := materializeSchemaValue(
				ctx, access, field, childValue,
				appendSchemaPath(path, schemaPathStep{index: index, isIndex: true}),
			)
			if err != nil {
				return nil, nil, err
			}
			result[index] = child
			entries = append(entries, childEntries...)
		}
		return result, entries, nil
	case string:
		if !capability.IsMaterializableSecretEnvelope(typed) {
			return typed, nil, nil
		}
		value, err := access.Materialize(ctx, field, typed)
		if err != nil {
			return nil, nil, err
		}
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil {
			return nil, nil, err
		}
		return descriptor.String(), []schemaSecret{{
			path: append([]schemaPathStep(nil), path...), value: value,
		}}, nil
	default:
		return typed, nil, nil
	}
}

func validateMaterializedSchema(
	field string,
	document map[string]any,
	secrets []schemaSecret,
) error {
	if document == nil || len(secrets) == 0 {
		return nil
	}
	resolved := cloneSchemaDocument(document)
	defer clearSchemaValue(resolved)
	return withResolvedSchemaSecrets(resolved, secrets, 0, func() error {
		_, err := compileRequestValidationSchema(field, resolved)
		if err != nil {
			return secret.ErrCredentialUnavailable
		}
		return nil
	})
}

func withResolvedSchemaSecrets(
	document map[string]any,
	secrets []schemaSecret,
	index int,
	use func() error,
) error {
	if index == len(secrets) {
		return use()
	}
	item := secrets[index]
	original, ok := schemaStringAtPath(document, item.path)
	if !ok {
		return secret.ErrCredentialUnavailable
	}
	return item.value.Use(func(plaintext string) (result error) {
		if !setSchemaPath(document, item.path, plaintext) {
			return secret.ErrCredentialUnavailable
		}
		defer func() {
			if !setSchemaPath(document, item.path, original) {
				result = secret.ErrCredentialUnavailable
			}
		}()
		return withResolvedSchemaSecrets(document, secrets, index+1, use)
	})
}

func appendSchemaPath(path []schemaPathStep, step schemaPathStep) []schemaPathStep {
	result := make([]schemaPathStep, len(path)+1)
	copy(result, path)
	result[len(path)] = step
	return result
}

func cloneSchemaDocument(document map[string]any) map[string]any {
	clone, _ := cloneSchemaValue(document).(map[string]any)
	return clone
}

func cloneSchemaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			result[key] = cloneSchemaValue(child)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, child := range typed {
			result[index] = cloneSchemaValue(child)
		}
		return result
	default:
		return typed
	}
}

func setSchemaPath(document map[string]any, path []schemaPathStep, value string) bool {
	if document == nil || len(path) == 0 {
		return false
	}
	var current any = document
	for index, step := range path {
		last := index == len(path)-1
		if step.isIndex {
			items, ok := current.([]any)
			if !ok || step.index < 0 || step.index >= len(items) {
				return false
			}
			if last {
				items[step.index] = value
				return true
			}
			current = items[step.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		if last {
			object[step.key] = value
			return true
		}
		child, ok := object[step.key]
		if !ok {
			return false
		}
		current = child
	}
	return false
}

func schemaStringAtPath(document map[string]any, path []schemaPathStep) (string, bool) {
	if document == nil || len(path) == 0 {
		return "", false
	}
	var current any = document
	for _, step := range path {
		if step.isIndex {
			items, ok := current.([]any)
			if !ok || step.index < 0 || step.index >= len(items) {
				return "", false
			}
			current = items[step.index]
			continue
		}
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		child, ok := object[step.key]
		if !ok {
			return "", false
		}
		current = child
	}
	value, ok := current.(string)
	return value, ok
}

func clearSchemaValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			clearSchemaValue(child)
			if _, ok := child.(string); ok {
				typed[key] = ""
			}
		}
	case []any:
		for index, child := range typed {
			clearSchemaValue(child)
			if _, ok := child.(string); ok {
				typed[index] = ""
			}
		}
	}
}
