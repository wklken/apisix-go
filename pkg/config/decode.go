package config

import (
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
)

type staticExtensions struct {
	RuntimePaths RuntimePaths `mapstructure:"runtime_paths"`
}

func decodeConfig(root *valueNode) (*Config, RuntimePaths, []string, error) {
	if root == nil || root.kind != nodeMapping {
		return nil, RuntimePaths{}, nil, fmt.Errorf("configuration root must be a mapping")
	}
	raw, ok := nodeToAny(root).(map[string]any)
	if !ok {
		return nil, RuntimePaths{}, nil, fmt.Errorf("configuration root must be a mapping")
	}
	mainRaw := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "apisix_go" {
			mainRaw[key] = value
		}
	}

	var cfg Config
	mainUnused, err := decodeMapstructure(mainRaw, &cfg)
	if err != nil {
		return nil, RuntimePaths{}, nil, fmt.Errorf("decode static configuration: %w", err)
	}
	if rawPlugins, exists := mainRaw["plugins"]; exists {
		plugins, valid := decodeHTTPPluginAllowlist(rawPlugins)
		if !valid {
			return nil, RuntimePaths{}, nil, fmt.Errorf("decode static configuration: plugins must be a string list")
		}
		cfg.Plugins = plugins
	}
	var extension staticExtensions
	extensionUnused, err := decodeMapstructure(raw["apisix_go"], &extension)
	if err != nil {
		return nil, RuntimePaths{}, nil, fmt.Errorf("decode apisix_go static configuration: %w", err)
	}
	for index := range extensionUnused {
		extensionUnused[index] = "apisix_go." + extensionUnused[index]
	}
	unused := append(mainUnused, extensionUnused...)
	unused = expandUnusedPaths(root, unused)
	sort.Strings(unused)
	unused = slices.Compact(unused)
	return &cfg, extension.RuntimePaths, unused, nil
}

func decodeMapstructure(input any, result any) ([]string, error) {
	if input == nil {
		return nil, nil
	}
	metadata := new(mapstructure.Metadata)
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.ComposeDecodeHookFunc(jsonNumberDecodeHook, configDecodeHook),
		WeaklyTypedInput: true,
		Metadata:         metadata,
		Result:           result,
		TagName:          "mapstructure",
		ZeroFields:       true,
	})
	if err != nil {
		return nil, err
	}
	if err := decoder.Decode(input); err != nil {
		return nil, err
	}
	return append([]string(nil), metadata.Unused...), nil
}

func jsonNumberDecodeHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from != reflect.TypeFor[json.Number]() {
		return data, nil
	}
	number := string(data.(json.Number))
	value := reflect.New(to).Elem()
	switch to.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(number, 10, to.Bits())
		if err != nil {
			return nil, fmt.Errorf("numeric value is not representable as %s", to)
		}
		value.SetInt(parsed)
		return value.Interface(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		parsed, err := strconv.ParseUint(number, 10, to.Bits())
		if err != nil {
			return nil, fmt.Errorf("numeric value is not representable as %s", to)
		}
		value.SetUint(parsed)
		return value.Interface(), nil
	default:
		return data, nil
	}
}

func configDecodeHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from.Kind() == reflect.String && to == reflect.TypeFor[time.Duration]() {
		duration, err := time.ParseDuration(strings.TrimSpace(data.(string)))
		if err != nil {
			return nil, fmt.Errorf("duration value is invalid")
		}
		return duration, nil
	}

	switch to {
	case reflect.TypeFor[NodeListen]():
		return decodeNodeListen(data)
	case reflect.TypeFor[TcpListen]():
		return decodeTCPListen(data)
	}
	if to.Kind() == reflect.Slice && to.Elem() == reflect.TypeFor[NodeListen]() {
		listen, err := decodeNodeListen(data)
		if err != nil {
			return nil, err
		}
		if value, ok := listen.(NodeListen); ok {
			return []NodeListen{value}, nil
		}
	}
	if to.Kind() == reflect.Slice && to.Elem() == reflect.TypeFor[TcpListen]() {
		listen, err := decodeTCPListen(data)
		if err != nil {
			return nil, err
		}
		if value, ok := listen.(TcpListen); ok {
			return []TcpListen{value}, nil
		}
	}

	if from.Kind() == reflect.String && to.Kind() == reflect.Slice && to.Elem().Kind() == reflect.String {
		value := strings.TrimSpace(data.(string))
		if value == "" {
			return []string{}, nil
		}
		if strings.Contains(value, ",") {
			parts := strings.Split(value, ",")
			for index := range parts {
				parts[index] = strings.TrimSpace(parts[index])
			}
			return parts, nil
		}
		return strings.Fields(value), nil
	}
	return data, nil
}

func decodeNodeListen(data any) (any, error) {
	port, host, ok, err := decodeListenAddress(data)
	if err != nil {
		return nil, err
	}
	if !ok {
		return data, nil
	}
	return NodeListen{Ip: host, Port: port}, nil
}

func decodeTCPListen(data any) (any, error) {
	port, host, ok, err := decodeListenAddress(data)
	if err != nil {
		return nil, err
	}
	if !ok {
		if address, isString := data.(string); isString {
			return TcpListen{Addr: strings.TrimSpace(address)}, nil
		}
		return data, nil
	}
	return TcpListen{Addr: net.JoinHostPort(host, strconv.Itoa(port))}, nil
}

func decodeListenAddress(data any) (port int, host string, ok bool, err error) {
	reflected := reflect.ValueOf(data)
	if reflected.IsValid() {
		switch reflected.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return int(reflected.Int()), "", true, nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return int(reflected.Uint()), "", true, nil
		case reflect.String:
			return decodeListenAddressString(reflected.String())
		}
	}
	return 0, "", false, nil
}

func decodeListenAddressString(address string) (port int, host string, ok bool, err error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return 0, "", true, nil
	}
	if port, err := strconv.Atoi(address); err == nil {
		return port, "", true, nil
	}
	if portString, found := strings.CutPrefix(address, ":"); found {
		port, err := strconv.Atoi(portString)
		if err != nil {
			return 0, "", true, fmt.Errorf("listener port must be a decimal integer")
		}
		return port, "", true, nil
	}
	parsedHost, parsedPort, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return 0, "", false, nil
	}
	port, err = strconv.Atoi(parsedPort)
	if err != nil {
		return 0, "", true, fmt.Errorf("listener port must be a decimal integer")
	}
	return port, parsedHost, true, nil
}

func expandUnusedPaths(root *valueNode, unused []string) []string {
	expanded := make([]string, 0, len(unused))
	for _, path := range unused {
		node := lookupStaticNode(root, path)
		if node == nil {
			expanded = append(expanded, path)
			continue
		}
		collectUnknownLeafPaths(node, path, &expanded)
	}
	return expanded
}

func collectUnknownLeafPaths(node *valueNode, path string, paths *[]string) {
	if node == nil {
		*paths = append(*paths, path)
		return
	}
	if len(node.mapping) == 0 && len(node.sequence) == 0 {
		*paths = append(*paths, path)
		return
	}
	keys := make([]string, 0, len(node.mapping))
	for key := range node.mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		collectUnknownLeafPaths(node.mapping[key], appendProvenanceKey(path, key), paths)
	}
	for index, child := range node.sequence {
		collectUnknownLeafPaths(child, fmt.Sprintf("%s[%d]", path, index), paths)
	}
}

func lookupStaticNode(root *valueNode, path string) *valueNode {
	current := root
	for segment := range strings.SplitSeq(path, ".") {
		if current == nil || current.kind != nodeMapping {
			return nil
		}
		current = current.mapping[segment]
	}
	return current
}
