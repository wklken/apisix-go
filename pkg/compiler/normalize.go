package compiler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/generation"
	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

func normalizeContext(ctx context.Context, snapshot generation.Snapshot) (normalizedInput, []resourceIssue, error) {
	input := newNormalizedInput(snapshot.Revision())
	input.collectionVersions = snapshot.CollectionVersions()
	issues := make([]resourceIssue, 0)
	typedIDs := make(map[string]map[string][]generation.ResourceKey)
	for _, source := range snapshot.Resources() {
		if err := ctx.Err(); err != nil {
			return normalizedInput{}, nil, err
		}
		key := source.Key
		raw := bytes.Clone(source.Value)
		resource := normalizedResource{key: key, origin: source.Origin, raw: raw}
		if !generation.IsManagedResourceKind(key.Kind) {
			issues = append(issues, newIssue(key, "unsupported-kind", "resource kind is not managed"))
		} else if key.Kind == "secrets" && !validSecretResourceID(key.ID) {
			issues = append(issues, newIssue(key, "malformed-secret-id", "secret id must be manager/id"))
		}

		document, err := decodeExactDocument(raw)
		if err != nil {
			issues = append(issues, resourceIssue{Key: key, Code: "decode-invalid", Err: err})
			input.resources[key] = resource
			continue
		}
		resource.document = document
		view, viewIssues := decodeStructuralView(key, document)
		resource.view = view
		issues = append(issues, viewIssues...)
		input.resources[key] = resource

		if view.hasEmbeddedID {
			byID := typedIDs[key.Kind]
			if byID == nil {
				byID = make(map[string][]generation.ResourceKey)
				typedIDs[key.Kind] = byID
			}
			byID[view.embeddedID] = append(byID[view.embeddedID], key)
		}
	}
	for _, byID := range typedIDs {
		if err := ctx.Err(); err != nil {
			return normalizedInput{}, nil, err
		}
		for _, keys := range byID {
			if len(keys) < 2 {
				continue
			}
			slices.SortFunc(keys, compareResourceKey)
			for _, key := range keys {
				issues = append(issues, newIssue(key, "duplicate-typed-id", "typed id is duplicated"))
			}
		}
	}
	for _, tombstone := range snapshot.Tombstones() {
		if err := ctx.Err(); err != nil {
			return normalizedInput{}, nil, err
		}
		input.tombstones[tombstone.Key] = tombstone
		if !generation.IsManagedResourceKind(tombstone.Key.Kind) {
			issues = append(issues, newIssue(tombstone.Key, "unsupported-kind", "tombstone kind is not managed"))
		} else if tombstone.Key.Kind == "secrets" && !validSecretResourceID(tombstone.Key.ID) {
			issues = append(issues, newIssue(tombstone.Key, "malformed-secret-id", "secret id must be manager/id"))
		}
	}
	if err := ctx.Err(); err != nil {
		return normalizedInput{}, nil, err
	}
	sortIssues(issues)
	return input, issues, nil
}

func typedResourceDocument(resource normalizedResource) any {
	object, ok := resource.document.(map[string]any)
	if !ok {
		return resource.document
	}
	result := maps.Clone(object)
	if resource.view.hasEmbeddedID {
		result["id"] = resource.view.embeddedID
	}
	setReference := func(field, value string) {
		if _, exists := result[field]; exists && value != "" {
			result[field] = value
		}
	}
	switch resource.key.Kind {
	case "routes", "stream_routes":
		setReference("service_id", resource.view.serviceID)
		setReference("upstream_id", resource.view.upstreamID)
		setReference("plugin_config_id", resource.view.pluginConfigID)
	case "services":
		setReference("upstream_id", resource.view.upstreamID)
	case "consumers":
		setReference("group_id", resource.view.consumerGroupID)
	}
	return result
}

func decodeExactDocument(raw []byte) (any, error) {
	decoder := apisixjson.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode resource: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode resource: multiple JSON values")
		}
		return nil, fmt.Errorf("decode resource trailer: %w", err)
	}
	return document, nil
}

func decodeStructuralView(
	key generation.ResourceKey,
	document any,
) (structuralView, []resourceIssue) {
	if key.Kind == "plugins" {
		return decodePluginsSingleton(key, document)
	}
	object, ok := document.(map[string]any)
	if !ok {
		return structuralView{}, []resourceIssue{newIssue(key, "decode-invalid", "resource must be a JSON object")}
	}
	view := structuralView{}
	idField := ""
	switch key.Kind {
	case "routes",
		"services",
		"upstreams",
		"global_rules",
		"plugin_configs",
		"consumer_groups",
		"protos",
		"ssls",
		"stream_routes":
		idField = "id"
	case "consumers":
		idField = "username"
	}
	issues := make([]resourceIssue, 0)
	if idField != "" {
		if rawID, exists := object[idField]; exists {
			id, valid := "", false
			if key.Kind == "consumers" {
				id, valid = rawID.(string)
			} else {
				id, valid = referenceID(rawID)
			}
			if !valid || id == "" {
				requirement := "a string or exact positive integer"
				if key.Kind == "consumers" {
					requirement = "a non-empty string"
				}
				issues = append(issues, newIssue(key, "id-invalid", idField+" must be "+requirement))
			} else {
				view.embeddedID = id
				view.hasEmbeddedID = true
				if id != key.ID {
					issues = append(issues, newIssue(key, "id-mismatch", "embedded id differs from resource key"))
				}
			}
		}
	}
	switch key.Kind {
	case "routes":
		view.serviceID, issues = optionalReferenceID(key, object, "service_id", issues)
		view.upstreamID, issues = optionalReferenceID(key, object, "upstream_id", issues)
		view.pluginConfigID, issues = optionalReferenceID(key, object, "plugin_config_id", issues)
		view.hasInlineUpstream, issues = inlineUpstreamPresence(key, object, issues)
	case "stream_routes":
		view.serviceID, issues = optionalReferenceID(key, object, "service_id", issues)
		view.upstreamID, issues = optionalReferenceID(key, object, "upstream_id", issues)
		view.hasInlineUpstream, issues = inlineUpstreamPresence(key, object, issues)
	case "services":
		view.upstreamID, issues = optionalReferenceID(key, object, "upstream_id", issues)
		view.hasInlineUpstream, issues = inlineUpstreamPresence(key, object, issues)
	case "consumers":
		view.consumerGroupID, issues = optionalReferenceID(key, object, "group_id", issues)
	}
	if resourceKindHasPlugins(key.Kind) {
		view.plugins, issues = decodePluginMap(key, object, issues)
	}
	return view, issues
}

func inlineUpstreamPresence(
	key generation.ResourceKey,
	object map[string]any,
	issues []resourceIssue,
) (bool, []resourceIssue) {
	upstream, exists := object["upstream"]
	if !exists {
		return false, issues
	}
	if _, valid := upstream.(map[string]any); !valid {
		return false, append(issues, newIssue(key, "upstream-invalid", "inline upstream must be an object"))
	}
	// Task 5 records higher-precedence inline intent only. Full Upstream schema
	// admission belongs to the later runtime/schema phase.
	return true, issues
}

func resourceKindHasPlugins(kind string) bool {
	switch kind {
	case "routes", "stream_routes", "services", "global_rules", "plugin_configs", "consumers", "consumer_groups":
		return true
	default:
		return false
	}
}

func decodePluginMap(
	key generation.ResourceKey,
	object map[string]any,
	issues []resourceIssue,
) (map[string]any, []resourceIssue) {
	plugins, exists := object["plugins"]
	if !exists || plugins == nil {
		return nil, issues
	}
	if typed, valid := plugins.(map[string]any); valid {
		return typed, issues
	}
	return nil, append(issues, newIssue(key, "plugins-invalid", "plugins must be an object"))
}

func decodePluginsSingleton(key generation.ResourceKey, document any) (structuralView, []resourceIssue) {
	values, ok := document.([]any)
	if !ok || values == nil || key.ID != "plugins" {
		return structuralView{}, []resourceIssue{
			newIssue(key, "malformed-singleton", "plugins must be the singleton plugins JSON array"),
		}
	}
	plugins := make(map[string]any, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			return structuralView{}, []resourceIssue{
				newIssue(key, "malformed-singleton", "plugin entries must be objects"),
			}
		}
		for field := range entry {
			if field != "name" && field != "stream" {
				return structuralView{}, []resourceIssue{
					newIssue(key, "malformed-singleton", "plugin entry contains an unknown field"),
				}
			}
		}
		name, ok := entry["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return structuralView{}, []resourceIssue{
				newIssue(key, "malformed-singleton", "plugin names must be non-empty strings"),
			}
		}
		if stream, exists := entry["stream"]; exists {
			if _, ok := stream.(bool); !ok {
				return structuralView{}, []resourceIssue{
					newIssue(key, "malformed-singleton", "plugin stream must be boolean"),
				}
			}
		}
		if _, exists := plugins[name]; exists {
			return structuralView{}, []resourceIssue{
				newIssue(key, "malformed-singleton", "plugin names must be unique"),
			}
		}
		plugins[name] = entry
	}
	return structuralView{plugins: plugins}, nil
}

func optionalReferenceID(
	key generation.ResourceKey,
	object map[string]any,
	field string,
	issues []resourceIssue,
) (string, []resourceIssue) {
	value, exists := object[field]
	if !exists || value == nil {
		return "", issues
	}
	id, valid := referenceID(value)
	if !valid || id == "" {
		return "", append(
			issues,
			newIssue(key, "reference-invalid", field+" must be a string or exact positive integer"),
		)
	}
	return id, issues
}

func referenceID(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case apisixjson.Number:
		text := typed.String()
		if text == "" {
			return "", false
		}
		value, valid := new(big.Rat).SetString(text)
		if !valid || !value.IsInt() || value.Sign() <= 0 {
			return "", false
		}
		return value.Num().String(), true
	default:
		return "", false
	}
}

func validSecretResourceID(id string) bool {
	parts := strings.Split(id, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func newIssue(key generation.ResourceKey, code, message string) resourceIssue {
	return resourceIssue{Key: key, Code: code, Err: errors.New(message)}
}

func sortIssues(issues []resourceIssue) {
	slices.SortStableFunc(issues, func(left, right resourceIssue) int {
		if byKey := compareResourceKey(left.Key, right.Key); byKey != 0 {
			return byKey
		}
		if byCode := strings.Compare(left.Code, right.Code); byCode != 0 {
			return byCode
		}
		if byDiagnostic := strings.Compare(left.Diagnostic, right.Diagnostic); byDiagnostic != 0 {
			return byDiagnostic
		}
		leftMessage, rightMessage := "", ""
		if left.Err != nil {
			leftMessage = left.Err.Error()
		}
		if right.Err != nil {
			rightMessage = right.Err.Error()
		}
		return strings.Compare(leftMessage, rightMessage)
	})
}

func compareResourceKey(left, right generation.ResourceKey) int {
	if byKind := strings.Compare(left.Kind, right.Kind); byKind != 0 {
		return byKind
	}
	return strings.Compare(left.ID, right.ID)
}
