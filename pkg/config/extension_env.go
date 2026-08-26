package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

const removedDeploymentProfileError = "deployment.profile was removed; use compatibility_target, security_profile, and qualification_profile"

var reservedRuntimePathAliases = map[string]string{
	"apisix_go.runtime_paths.data_dir":    "APISIXGO_RUNTIME_PATHS_DATA_DIR",
	"apisix_go.runtime_paths.runtime_dir": "APISIXGO_RUNTIME_PATHS_RUNTIME_DIR",
	"apisix_go.runtime_paths.log_dir":     "APISIXGO_RUNTIME_PATHS_LOG_DIR",
	"apisix_go.runtime_paths.temp_dir":    "APISIXGO_RUNTIME_PATHS_TEMP_DIR",
}

type staticSchemaEntry struct {
	path  string
	alias string
}

type staticSchemaIndex struct {
	byPath  map[string]string
	byAlias map[string]string
}

var configStaticSchema, configStaticSchemaErr = buildStaticSchemaIndex(reflect.TypeFor[Config]())

func buildStaticSchemaIndex(configType reflect.Type) (*staticSchemaIndex, error) {
	entries := make([]staticSchemaEntry, 0)
	collectStaticSchemaEntries(configType, "", &entries)
	for path, alias := range reservedRuntimePathAliases {
		entries = append(entries, staticSchemaEntry{path: path, alias: alias})
	}

	slices.SortFunc(entries, func(left, right staticSchemaEntry) int {
		if order := strings.Compare(left.path, right.path); order != 0 {
			return order
		}
		return strings.Compare(left.alias, right.alias)
	})
	for index := 1; index < len(entries); index++ {
		if entries[index-1].path == entries[index].path {
			return nil, fmt.Errorf("static configuration path collision: %s", entries[index].path)
		}
	}

	aliasEntries := append([]staticSchemaEntry(nil), entries...)
	slices.SortFunc(aliasEntries, func(left, right staticSchemaEntry) int {
		if order := strings.Compare(left.alias, right.alias); order != 0 {
			return order
		}
		return strings.Compare(left.path, right.path)
	})
	for index := 1; index < len(aliasEntries); index++ {
		previous, current := aliasEntries[index-1], aliasEntries[index]
		if previous.alias == current.alias {
			return nil, fmt.Errorf(
				"static configuration environment alias collision: %s maps to %s and %s",
				current.alias, previous.path, current.path,
			)
		}
	}

	result := &staticSchemaIndex{
		byPath:  make(map[string]string, len(entries)),
		byAlias: make(map[string]string, len(entries)),
	}
	for _, entry := range entries {
		result.byPath[entry.path] = entry.alias
		result.byAlias[entry.alias] = entry.path
	}
	return result, nil
}

func collectStaticSchemaEntries(configType reflect.Type, parent string, entries *[]staticSchemaEntry) {
	if configType.Kind() != reflect.Struct {
		return
	}
	for field := range configType.Fields() {
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("mapstructure"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		path := name
		if parent != "" {
			path = parent + "." + name
		}
		if field.Type.Kind() == reflect.Struct {
			collectStaticSchemaEntries(field.Type, path, entries)
			continue
		}
		*entries = append(*entries, staticSchemaEntry{path: path, alias: extensionEnvName(path)})
	}
}

func extensionEnvName(path string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return "APISIXGO_" + strings.ToUpper(replacer.Replace(path))
}

func ValidateStaticOverridePath(path string) error {
	if err := validateConfigurationPath(path); err != nil {
		return errors.New("configuration path is invalid")
	}
	if path == "deployment.profile" {
		return fmt.Errorf("%s", removedDeploymentProfileError)
	}
	if configStaticSchemaErr != nil {
		return fmt.Errorf("build static configuration schema: %w", configStaticSchemaErr)
	}
	if _, ok := configStaticSchema.byPath[path]; !ok {
		return errors.New("configuration path does not map to a static configuration field")
	}
	return nil
}

func applyAPISIXGO(root *valueNode, environment map[string]string) error {
	if configStaticSchemaErr != nil {
		return fmt.Errorf("build static configuration schema: %w", configStaticSchemaErr)
	}

	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	slices.Sort(names)
	operations := make([]staticOverlayOperation, 0, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, "APISIXGO_") {
			continue
		}
		if name == "APISIXGO_DEPLOYMENT_PROFILE" {
			return fmt.Errorf("%s", removedDeploymentProfileError)
		}
		path, ok := configStaticSchema.byAlias[name]
		if !ok {
			return fmt.Errorf("%s does not map to a static configuration field", name)
		}
		operations = append(operations, staticOverlayOperation{
			path: path,
			value: &valueNode{kind: nodeScalar, scalar: environment[name], source: FieldSource{
				Kind: SourceAPISIXGOEnv, Origin: name, Explicit: true,
			}},
		})
	}
	return applyStaticOverlayOperations(root, operations)
}

func applyCLIOverrides(root *valueNode, overrides map[string]any) error {
	paths := make([]string, 0, len(overrides))
	for path := range overrides {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	operations := make([]staticOverlayOperation, 0, len(paths))
	for _, path := range paths {
		if err := ValidateStaticOverridePath(path); err != nil {
			return err
		}
		node, err := nodeFromAny(overrides[path], FieldSource{Kind: SourceCLI, Origin: path, Explicit: true})
		if err != nil {
			return err
		}
		operations = append(operations, staticOverlayOperation{path: path, value: node})
	}
	return applyStaticOverlayOperations(root, operations)
}

type staticOverlayOperation struct {
	path  string
	value *valueNode
}

func applyStaticOverlayOperations(root *valueNode, operations []staticOverlayOperation) error {
	working := cloneNode(root)
	for _, operation := range operations {
		if err := setPath(working, operation.path, operation.value); err != nil {
			return err
		}
	}
	if len(operations) == 0 {
		return nil
	}
	*root = *working
	return nil
}

func setPath(root *valueNode, path string, value *valueNode) error {
	if root == nil || root.kind != nodeMapping {
		return fmt.Errorf("configuration root must be a mapping")
	}
	if err := validateConfigurationPath(path); err != nil {
		return err
	}
	if root.mapping == nil {
		root.mapping = make(map[string]*valueNode)
	}
	segments := strings.Split(path, ".")
	current := root
	for _, segment := range segments[:len(segments)-1] {
		next := current.mapping[segment]
		if next == nil {
			next = &valueNode{
				kind: nodeMapping, mapping: make(map[string]*valueNode),
				source: value.source, pathBase: value.pathBase,
			}
			current.mapping[segment] = next
		}
		if next.kind != nodeMapping {
			return fmt.Errorf("configuration path %s crosses a non-mapping value", path)
		}
		if next.mapping == nil {
			next.mapping = make(map[string]*valueNode)
		}
		current = next
	}
	current.mapping[segments[len(segments)-1]] = cloneNode(value)
	return nil
}

func validateConfigurationPath(path string) error {
	if path == "" {
		return fmt.Errorf("configuration path %q is empty", path)
	}
	if slices.Contains(strings.Split(path, "."), "") {
		return fmt.Errorf("configuration path %q contains an empty segment", path)
	}
	return nil
}
