package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const redactedValue = "[REDACTED]"

type profileDump struct {
	Compatibility CompatibilityTarget  `json:"compatibility_target"`
	Security      SecurityProfile      `json:"security_profile"`
	Qualification QualificationProfile `json:"qualification_profile"`
}

type provenanceEntry struct {
	Path     string     `json:"path"`
	Kind     SourceKind `json:"kind"`
	Origin   string     `json:"origin"`
	Explicit bool       `json:"explicit"`
}

type effectiveDump struct {
	Config        map[string]any    `json:"config"`
	Paths         RuntimePaths      `json:"paths"`
	Profiles      profileDump       `json:"profiles"`
	Provenance    []provenanceEntry `json:"provenance"`
	IgnoredFields []string          `json:"ignored_fields"`
}

type canonicalTokenKind uint8

const (
	canonicalMappingToken canonicalTokenKind = iota
	canonicalIndexToken
)

type canonicalToken struct {
	kind  canonicalTokenKind
	name  string
	index uint64
}

type renderContext struct {
	effective *EffectiveConfig
	secrets   map[string]string
	opaqueIDs map[string]string
}

// RenderEffectiveRedacted renders an EffectiveConfig produced by LoadEffective
// without exposing registered secrets, unknown configuration fields, or
// APISIX-expanded keys. Approved plugin and discovery provider names remain
// visible; their complete configuration values are replaced with a marker.
func RenderEffectiveRedacted(effective *EffectiveConfig) ([]byte, error) {
	if effective == nil {
		return nil, errors.New("render effective config: config is required")
	}
	secrets, err := secretInventory(reflect.TypeFor[Config]())
	if err != nil {
		return nil, fmt.Errorf("render effective config: %w", err)
	}
	paths := collectOpaquePaths(effective, secrets)
	ctx := renderContext{
		effective: effective,
		secrets:   secrets,
		opaqueIDs: buildOpaquePathIDs(paths),
	}
	configValue, err := ctx.renderValue(reflect.ValueOf(effective.Config), reflect.TypeFor[Config](), "", "")
	if err != nil {
		return nil, fmt.Errorf("render effective config: %w", err)
	}
	configMap, ok := configValue.(map[string]any)
	if !ok {
		return nil, errors.New("render effective config: config is not a mapping")
	}
	provenance, ignored := ctx.renderProvenance()
	dump := effectiveDump{
		Config: configMap,
		Paths:  effective.Paths,
		Profiles: profileDump{
			Compatibility: effective.Profiles.Compatibility,
			Security:      effective.Profiles.Security,
			Qualification: effective.Profiles.Qualification,
		},
		Provenance:    provenance,
		IgnoredFields: ignored,
	}
	data, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("render effective config: %w", err)
	}
	return append(data, '\n'), nil
}

func validateSecretInventory(configType reflect.Type) error {
	_, err := secretInventory(configType)
	return err
}

func secretInventory(configType reflect.Type) (map[string]string, error) {
	if configType == nil {
		return nil, errors.New("secret inventory: configuration type is required")
	}
	result := make(map[string]string)
	if err := collectSecretFields(configType, "", result); err != nil {
		return nil, err
	}
	expected := map[string]string{
		"apisix.data_encryption.keyring":   "true",
		"deployment.admin.admin_key[].key": "true",
		"deployment.etcd.password":         "true",
		"deployment.etcd.host":             "url-userinfo",
		"plugin_attr":                      "container",
		"discovery":                        "container",
	}
	if len(result) != len(expected) {
		return nil, errors.New("secret inventory: registry does not match the required static fields")
	}
	for path, tag := range expected {
		if result[path] != tag {
			return nil, errors.New("secret inventory: registry does not match the required static fields")
		}
	}
	return result, nil
}

func collectSecretFields(configType reflect.Type, parent string, result map[string]string) error {
	for configType.Kind() == reflect.Pointer {
		configType = configType.Elem()
	}
	switch configType.Kind() {
	case reflect.Struct:
		for field := range configType.Fields() {
			if field.PkgPath != "" {
				continue
			}
			name := mapstructureFieldName(field)
			if name == "-" {
				continue
			}
			path := joinSchemaPath(parent, name)
			tag := field.Tag.Get("secret")
			if tag != "" {
				if tag != "true" && tag != "container" && tag != "url-userinfo" {
					return fmt.Errorf("secret inventory: unsupported tag at %s", path)
				}
				if !supportedSecretShape(field.Type, tag) {
					return fmt.Errorf("secret inventory: unsupported shape at %s", path)
				}
				if _, exists := result[path]; exists {
					return fmt.Errorf("secret inventory: duplicate path at %s", path)
				}
				result[path] = tag
			}
			childPath := path
			fieldType := field.Type
			for fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if fieldType.Kind() == reflect.Slice || fieldType.Kind() == reflect.Array {
				childPath += "[]"
				fieldType = fieldType.Elem()
				for fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}
			}
			if fieldType.Kind() == reflect.Struct {
				if err := collectSecretFields(fieldType, childPath, result); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func supportedSecretShape(fieldType reflect.Type, tag string) bool {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	switch tag {
	case "true":
		if fieldType.Kind() == reflect.Slice || fieldType.Kind() == reflect.Array {
			return true
		}
		return fieldType.Kind() != reflect.Struct && fieldType.Kind() != reflect.Map &&
			fieldType.Kind() != reflect.Interface && fieldType.Kind() != reflect.Func
	case "container":
		return fieldType.Kind() == reflect.Map && fieldType.Key().Kind() == reflect.String
	case "url-userinfo":
		if fieldType.Kind() != reflect.Slice && fieldType.Kind() != reflect.Array {
			return false
		}
		element := fieldType.Elem()
		for element.Kind() == reflect.Pointer {
			element = element.Elem()
		}
		return element.Kind() == reflect.String
	default:
		return false
	}
}

func mapstructureFieldName(field reflect.StructField) string {
	name := strings.Split(field.Tag.Get("mapstructure"), ",")[0]
	if name == "" {
		return field.Name
	}
	return name
}

func joinSchemaPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func (ctx renderContext) renderValue(value reflect.Value, valueType reflect.Type, path, tag string) (any, error) {
	if !value.IsValid() {
		return nil, nil
	}
	if tag != "" {
		switch tag {
		case "true":
			return ctx.renderSecretValue(value, valueType, path)
		case "container":
			return ctx.renderSecretContainer(value, path)
		case "url-userinfo":
			return ctx.renderURLUserinfo(value, path)
		default:
			return nil, errors.New("unsupported secret tag")
		}
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Struct:
		result := make(map[string]any, value.NumField())
		for fieldInfo := range value.Type().Fields() {
			if fieldInfo.PkgPath != "" {
				continue
			}
			name := mapstructureFieldName(fieldInfo)
			if name == "-" {
				continue
			}
			childPath := joinSchemaPath(path, name)
			child, err := ctx.renderValue(
				value.FieldByIndex(fieldInfo.Index), fieldInfo.Type, childPath, fieldInfo.Tag.Get("secret"),
			)
			if err != nil {
				return nil, err
			}
			result[name] = child
		}
		return result, nil
	case reflect.Map:
		if value.IsNil() {
			return nil, nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return nil, errors.New("unsupported map key shape")
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(left, right int) bool { return keys[left].String() < keys[right].String() })
		result := make(map[string]any, len(keys))
		for _, key := range keys {
			rawKey := key.String()
			childPath := redactionMapPath(path, rawKey)
			displayKey := ctx.dynamicMapKey(childPath, rawKey)
			childValue := value.MapIndex(key)
			child, err := ctx.renderValue(childValue, childValue.Type(), childPath, "")
			if err != nil {
				return nil, err
			}
			result[displayKey] = child
		}
		return result, nil
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil, nil
		}
		result := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			childPath := fmt.Sprintf("%s[%d]", path, index)
			child, err := ctx.renderValue(value.Index(index), value.Type().Elem(), childPath, "")
			if err != nil {
				return nil, err
			}
			result[index] = child
		}
		return result, nil
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return value.Interface(), nil
	default:
		return nil, errors.New("unsupported configuration field shape")
	}
}

func (ctx renderContext) renderSecretValue(value reflect.Value, valueType reflect.Type, path string) (any, error) {
	if secretValueEmpty(value) {
		return ctx.renderValue(value, valueType, path, "")
	}
	return redactedValue, nil
}

func secretValueEmpty(value reflect.Value) bool {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return true
		}
		value = value.Elem()
	}
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		return value.Len() == 0
	default:
		return false
	}
}

func (ctx renderContext) renderSecretContainer(value reflect.Value, path string) (any, error) {
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Map || value.Type().Key().Kind() != reflect.String {
		return nil, errors.New("unsupported secret container shape")
	}
	if value.IsNil() {
		return nil, nil
	}
	keys := value.MapKeys()
	sort.Slice(keys, func(left, right int) bool { return keys[left].String() < keys[right].String() })
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		rawKey := key.String()
		childPath := redactionMapPath(path, rawKey)
		result[ctx.secretContainerKey(childPath, rawKey)] = redactedValue
	}
	return result, nil
}

func (ctx renderContext) renderURLUserinfo(value reflect.Value, path string) (any, error) {
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return nil, errors.New("unsupported URL secret shape")
	}
	result := make([]any, value.Len())
	for index := 0; index < value.Len(); index++ {
		element := value.Index(index)
		for element.Kind() == reflect.Interface || element.Kind() == reflect.Pointer {
			if element.IsNil() {
				return nil, fmt.Errorf("unsupported URL secret at %s[%d]", path, index)
			}
			element = element.Elem()
		}
		if element.Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported URL secret at %s[%d]", path, index)
		}
		result[index] = sanitizeEtcdEndpoint(element.String())
	}
	return result, nil
}

func sanitizeEtcdEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Opaque != "" || !utf8.ValidString(endpoint) {
		return redactedValue
	}
	base := parsed.Scheme + "://"
	if parsed.User != nil {
		base += redactedValue + "@"
	}
	base += parsed.Host
	rootPath := parsed.Path == "" || parsed.Path == "/"
	if rootPath && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.RawPath == "" {
		if parsed.User == nil {
			return endpoint
		}
		return base
	}
	return base + "/<redacted>"
}

func (ctx renderContext) dynamicMapKey(path, rawKey string) string {
	if source, ok := ctx.effective.Provenance[path]; ok && source.Kind == SourceAPISIXEnv {
		return "apisix_env:" + ctx.opaqueID(path)
	}
	if safeProvenanceSegment.MatchString(rawKey) {
		return rawKey
	}
	return "opaque:" + ctx.opaqueID(path)
}

func redactionMapPath(parent, key string) string {
	if utf8.ValidString(key) {
		return appendProvenanceKey(parent, key)
	}
	// Invalid keys cannot be part of a canonical provenance path. Keep an
	// internal-only discriminator so separate invalid keys receive separate
	// opaque handles without ever entering JSON or an error message.
	return parent + "\x00" + key
}

func (ctx renderContext) secretContainerKey(path, rawKey string) string {
	if source, ok := ctx.effective.Provenance[path]; ok && source.Kind == SourceAPISIXEnv {
		return "apisix_env:" + ctx.opaqueID(path)
	}
	if safeProvenanceSegment.MatchString(rawKey) {
		return rawKey
	}
	return "plugin:" + ctx.opaqueID(path)
}

func (ctx renderContext) opaqueID(path string) string {
	if id, ok := ctx.opaqueIDs[path]; ok {
		return id
	}
	return "opaque:0000"
}

func (ctx renderContext) renderProvenance() ([]provenanceEntry, []string) {
	keys := make([]string, 0, len(ctx.effective.Provenance))
	for path := range ctx.effective.Provenance {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	entries := make([]provenanceEntry, 0, len(keys))
	ignoredSet := make(map[string]struct{})
	for _, rawPath := range keys {
		source := ctx.effective.Provenance[rawPath]
		displayPath, known := ctx.displayProvenancePath(rawPath, source)
		if !known {
			ignoredSet[displayPath] = struct{}{}
		}
		entries = append(entries, provenanceEntry{
			Path:     displayPath,
			Kind:     safeSourceKind(source.Kind),
			Origin:   safeSourceOrigin(source),
			Explicit: source.Explicit,
		})
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Path != entries[right].Path {
			return entries[left].Path < entries[right].Path
		}
		if entries[left].Kind != entries[right].Kind {
			return entries[left].Kind < entries[right].Kind
		}
		if entries[left].Origin != entries[right].Origin {
			return entries[left].Origin < entries[right].Origin
		}
		if entries[left].Explicit != entries[right].Explicit {
			return !entries[left].Explicit
		}
		return false
	})
	ignored := make([]string, 0, len(ignoredSet))
	for path := range ignoredSet {
		ignored = append(ignored, path)
	}
	sort.Strings(ignored)
	return entries, ignored
}

func safeSourceKind(kind SourceKind) SourceKind {
	switch kind {
	case SourceBuiltin, SourceDefaultFile, SourceOverrideFile, SourceAPISIXEnv, SourceAPISIXGOEnv, SourceCLI:
		return kind
	default:
		return SourceKind("unknown")
	}
}

func safeSourceOrigin(source FieldSource) string {
	switch source.Kind {
	case SourceBuiltin:
		if source.Origin == "apisix-go-runtime-defaults" {
			return source.Origin
		}
	case SourceDefaultFile, SourceOverrideFile:
		if source.Origin != "" && utf8.ValidString(source.Origin) {
			return source.Origin
		}
	case SourceAPISIXEnv:
		if redactionValidEnvironmentNames(source.Origin) {
			return source.Origin
		}
	case SourceAPISIXGOEnv:
		if redactionValidEnvironmentName(source.Origin) && strings.HasPrefix(source.Origin, "APISIXGO_") {
			return source.Origin
		}
	case SourceCLI:
		if err := ValidateStaticOverridePath(source.Origin); err == nil {
			return source.Origin
		}
	}
	return ""
}

func redactionValidEnvironmentNames(origin string) bool {
	names := strings.Split(origin, ",")
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if !redactionValidEnvironmentName(name) {
			return false
		}
	}
	return true
}

func redactionValidEnvironmentName(name string) bool {
	if name == "" || !utf8.ValidString(name) {
		return false
	}
	for index, char := range name {
		if index == 0 {
			if !redactionEnvironmentStart(char) {
				return false
			}
			continue
		}
		if !redactionEnvironmentPart(char) {
			return false
		}
	}
	return true
}

func redactionEnvironmentStart(char rune) bool {
	return (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || char == '_'
}

func redactionEnvironmentPart(char rune) bool {
	return redactionEnvironmentStart(char) || (char >= '0' && char <= '9')
}

func (ctx renderContext) displayProvenancePath(rawPath string, source FieldSource) (string, bool) {
	tokens, err := parseCanonicalPath(rawPath)
	if err != nil || !knownConfigTokens(tokens) {
		return "unknown:" + ctx.opaqueID(rawPath), false
	}
	if source.Kind == SourceAPISIXEnv {
		return "apisix_env:" + ctx.opaqueID(rawPath), true
	}
	for containerPath, tag := range ctx.secrets {
		if tag != "container" {
			continue
		}
		containerTokens, parseErr := parseCanonicalPath(containerPath)
		if parseErr != nil || len(tokens) <= len(containerTokens) || !tokensHavePrefix(tokens, containerTokens) {
			continue
		}
		pluginToken := tokens[len(containerTokens)]
		if pluginToken.kind != canonicalMappingToken {
			return "unknown:" + ctx.opaqueID(rawPath), false
		}
		pluginPath := canonicalPath(containerTokens)
		pluginPath = appendCanonicalToken(pluginPath, pluginToken)
		pluginDisplay := pluginToken.name
		if !safeProvenanceSegment.MatchString(pluginToken.name) {
			pluginDisplay = "plugin:" + ctx.opaqueID(pluginPath)
		}
		base := containerPath + "." + pluginDisplay
		if len(tokens) == len(containerTokens)+1 {
			return base, true
		}
		return base + ".redacted:" + ctx.opaqueID(rawPath), true
	}
	return rawPath, true
}

func tokensHavePrefix(tokens, prefix []canonicalToken) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for index := range prefix {
		if tokens[index] != prefix[index] {
			return false
		}
	}
	return true
}

func canonicalPath(tokens []canonicalToken) string {
	path := ""
	for _, token := range tokens {
		path = appendCanonicalToken(path, token)
	}
	return path
}

func appendCanonicalToken(path string, token canonicalToken) string {
	if token.kind == canonicalIndexToken {
		return path + "[" + strconv.FormatUint(token.index, 10) + "]"
	}
	if safeProvenanceSegment.MatchString(token.name) {
		if path == "" {
			return token.name
		}
		return path + "." + token.name
	}
	encoded, _ := json.Marshal(token.name)
	return path + "[" + string(encoded) + "]"
}

func parseCanonicalPath(path string) ([]canonicalToken, error) {
	if path == "" || !utf8.ValidString(path) {
		return nil, errors.New("invalid canonical configuration path")
	}
	tokens := make([]canonicalToken, 0, 4)
	position := 0
	afterDot := false
	for position < len(path) {
		if path[position] == '[' {
			if position+1 < len(path) && path[position+1] == '"' {
				if afterDot {
					return nil, errors.New("invalid canonical configuration path")
				}
				name, next, err := parseCanonicalStringToken(path, position)
				if err != nil {
					return nil, err
				}
				tokens = append(tokens, canonicalToken{kind: canonicalMappingToken, name: name})
				position = next
			} else {
				if afterDot {
					return nil, errors.New("invalid canonical configuration path")
				}
				index, next, err := parseCanonicalIndexToken(path, position)
				if err != nil || len(tokens) == 0 {
					return nil, errors.New("invalid canonical configuration path")
				}
				tokens = append(tokens, canonicalToken{kind: canonicalIndexToken, index: index})
				position = next
			}
		} else {
			start := position
			for position < len(path) && path[position] != '.' && path[position] != '[' {
				position++
			}
			name := path[start:position]
			if name == "" || !safeProvenanceSegment.MatchString(name) {
				return nil, errors.New("invalid canonical configuration path")
			}
			tokens = append(tokens, canonicalToken{kind: canonicalMappingToken, name: name})
		}
		if position == len(path) {
			break
		}
		switch path[position] {
		case '.':
			position++
			if position == len(path) {
				return nil, errors.New("invalid canonical configuration path")
			}
			afterDot = true
		case '[':
			afterDot = false
		default:
			return nil, errors.New("invalid canonical configuration path")
		}
	}
	if len(tokens) == 0 || tokens[0].kind != canonicalMappingToken {
		return nil, errors.New("invalid canonical configuration path")
	}
	return tokens, nil
}

func parseCanonicalStringToken(path string, start int) (string, int, error) {
	position := start + 1
	if position >= len(path) || path[position] != '"' {
		return "", 0, errors.New("invalid canonical configuration path")
	}
	position++
	escaped := false
	for ; position < len(path); position++ {
		char := path[position]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			if position+1 >= len(path) || path[position+1] != ']' {
				return "", 0, errors.New("invalid canonical configuration path")
			}
			var name string
			if err := json.Unmarshal([]byte(path[start+1:position+1]), &name); err != nil ||
				!utf8.ValidString(name) || safeProvenanceSegment.MatchString(name) {
				return "", 0, errors.New("invalid canonical configuration path")
			}
			return name, position + 2, nil
		}
	}
	return "", 0, errors.New("invalid canonical configuration path")
}

func parseCanonicalIndexToken(path string, start int) (uint64, int, error) {
	end := strings.IndexByte(path[start+1:], ']')
	if end < 0 {
		return 0, 0, errors.New("invalid canonical configuration path")
	}
	end += start + 1
	raw := path[start+1 : end]
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, 0, errors.New("invalid canonical configuration path")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, 0, errors.New("invalid canonical configuration path")
		}
	}
	index, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, 0, errors.New("invalid canonical configuration path")
	}
	return index, end + 1, nil
}

func knownConfigTokens(tokens []canonicalToken) bool {
	if len(tokens) == 0 {
		return false
	}
	if tokens[0].kind == canonicalMappingToken && tokens[0].name == "apisix_go" {
		return knownRuntimePathTokens(tokens[1:])
	}
	return matchConfigType(reflect.TypeFor[Config](), tokens)
}

func knownRuntimePathTokens(tokens []canonicalToken) bool {
	if len(tokens) == 0 {
		return true
	}
	if tokens[0].kind != canonicalMappingToken || tokens[0].name != "runtime_paths" {
		return false
	}
	if len(tokens) == 1 {
		return true
	}
	if len(tokens) != 2 || tokens[1].kind != canonicalMappingToken {
		return false
	}
	switch tokens[1].name {
	case "data_dir", "runtime_dir", "log_dir", "temp_dir":
		return true
	default:
		return false
	}
}

func matchConfigType(configType reflect.Type, tokens []canonicalToken) bool {
	for configType.Kind() == reflect.Pointer {
		configType = configType.Elem()
	}
	if len(tokens) == 0 {
		return true
	}
	switch configType.Kind() {
	case reflect.Struct:
		if tokens[0].kind != canonicalMappingToken || !safeProvenanceSegment.MatchString(tokens[0].name) {
			return false
		}
		for field := range configType.Fields() {
			if field.PkgPath != "" || mapstructureFieldName(field) != tokens[0].name ||
				mapstructureFieldName(field) == "-" {
				continue
			}
			return matchConfigType(field.Type, tokens[1:])
		}
		return false
	case reflect.Map:
		if configType.Key().Kind() != reflect.String || tokens[0].kind != canonicalMappingToken {
			return false
		}
		return matchConfigType(configType.Elem(), tokens[1:])
	case reflect.Slice, reflect.Array:
		if tokens[0].kind != canonicalIndexToken {
			return false
		}
		if configType.Kind() == reflect.Array && tokens[0].index >= uint64(configType.Len()) {
			return false
		}
		return matchConfigType(configType.Elem(), tokens[1:])
	case reflect.Interface:
		return true
	default:
		return false
	}
}

func collectOpaquePaths(effective *EffectiveConfig, secrets map[string]string) []string {
	paths := make(map[string]struct{})
	for rawPath, source := range effective.Provenance {
		if source.Kind == SourceAPISIXEnv {
			paths[rawPath] = struct{}{}
		}
		tokens, err := parseCanonicalPath(rawPath)
		if err != nil || !knownConfigTokens(tokens) {
			paths[rawPath] = struct{}{}
			continue
		}
		for containerPath, tag := range secrets {
			if tag != "container" {
				continue
			}
			containerTokens, parseErr := parseCanonicalPath(containerPath)
			if parseErr != nil || !tokensHavePrefix(tokens, containerTokens) || len(tokens) <= len(containerTokens) {
				continue
			}
			pluginToken := tokens[len(containerTokens)]
			pluginPath := appendCanonicalToken(canonicalPath(containerTokens), pluginToken)
			if !safeProvenanceSegment.MatchString(pluginToken.name) {
				paths[pluginPath] = struct{}{}
			}
			if len(tokens) > len(containerTokens)+1 {
				paths[rawPath] = struct{}{}
			}
		}
	}
	collectConfigOpaquePaths(
		reflect.ValueOf(effective.Config), "", "", effective.Provenance, paths,
	)
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	return result
}

func collectConfigOpaquePaths(
	value reflect.Value, path, tag string, provenance Provenance, paths map[string]struct{},
) {
	if !value.IsValid() {
		return
	}
	if tag == "container" {
		for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.Map || value.IsNil() {
			return
		}
		for _, key := range value.MapKeys() {
			rawKey := key.String()
			childPath := redactionMapPath(path, rawKey)
			if source, ok := provenance[childPath]; ok && source.Kind == SourceAPISIXEnv {
				paths[childPath] = struct{}{}
			}
			if !safeProvenanceSegment.MatchString(rawKey) {
				paths[childPath] = struct{}{}
			}
		}
		return
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Struct:
		for field := range value.Type().Fields() {
			if field.PkgPath != "" {
				continue
			}
			name := mapstructureFieldName(field)
			if name == "-" {
				continue
			}
			collectConfigOpaquePaths(
				value.FieldByIndex(field.Index), joinSchemaPath(path, name), field.Tag.Get("secret"), provenance, paths,
			)
		}
	case reflect.Map:
		if value.IsNil() || value.Type().Key().Kind() != reflect.String {
			return
		}
		for _, key := range value.MapKeys() {
			rawKey := key.String()
			childPath := redactionMapPath(path, rawKey)
			if source, ok := provenance[childPath]; ok && source.Kind == SourceAPISIXEnv {
				paths[childPath] = struct{}{}
			}
			if !safeProvenanceSegment.MatchString(rawKey) {
				paths[childPath] = struct{}{}
			}
			collectConfigOpaquePaths(value.MapIndex(key), childPath, "", provenance, paths)
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			collectConfigOpaquePaths(
				value.Index(index), fmt.Sprintf("%s[%d]", path, index), "", provenance, paths,
			)
		}
	}
}

func buildOpaquePathIDs(paths []string) map[string]string {
	unique := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		unique[path] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for path := range unique {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	width := 4
	if candidate := len(strconv.Itoa(len(ordered))); candidate > width {
		width = candidate
	}
	result := make(map[string]string, len(ordered))
	for index, path := range ordered {
		result[path] = fmt.Sprintf("opaque:%0*d", width, index+1)
	}
	return result
}
