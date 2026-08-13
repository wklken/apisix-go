package config

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/wklken/apisix-go/pkg/data_encryption"
)

var GlobalConfig *Config

func Load() (*Config, error) {
	v := viper.GetViper()
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("fail to load config file, %w", err)
	}
	return load(v)
}

func load(v *viper.Viper) (*Config, error) {
	rawPlugins := v.Get("plugins")

	var cfg Config
	err := v.Unmarshal(&cfg, viper.DecodeHook(configDecodeHook))
	if err != nil {
		return nil, err
	}
	if rawPlugins != nil {
		if plugins, ok := decodeHTTPPluginAllowlist(rawPlugins); ok {
			cfg.Plugins = plugins
		}
	}
	if err := validateHTTPPluginAllowlist(cfg.Plugins); err != nil {
		return nil, err
	}
	if sendTimeout := cfg.NginxConfig.HTTP.SendTimeout; sendTimeout != 0 {
		return nil, fmt.Errorf(
			"nginx_config.http.send_timeout must be zero because Go cannot implement NGINX write-idle semantics, got %s",
			sendTimeout,
		)
	}

	GlobalConfig = &cfg
	data_encryption.Configure(cfg.Apisix.DataEncryption.EnableEncryptFields, cfg.Apisix.DataEncryption.Keyring)

	for _, limit := range []struct {
		field string
		value int
	}{
		{field: "proxy.max_idle_conns", value: cfg.Proxy.MaxIdleConns},
		{field: "proxy.max_idle_conns_per_host", value: cfg.Proxy.MaxIdleConnsPerHost},
		{field: "proxy.max_conns_per_host", value: cfg.Proxy.MaxConnsPerHost},
		{field: "proxy.max_in_flight", value: cfg.Proxy.MaxInFlight},
	} {
		if limit.value < 0 {
			return nil, fmt.Errorf("%s must be non-negative, got %d", limit.field, limit.value)
		}
	}

	return &cfg, nil
}

func decodeHTTPPluginAllowlist(raw any) ([]string, bool) {
	switch value := raw.(type) {
	case string:
		if strings.Contains(value, ",") {
			return strings.Split(value, ","), true
		}
		if value == "" || value != strings.TrimSpace(value) {
			return []string{value}, true
		}
		return strings.Fields(value), true
	case []string:
		return append([]string(nil), value...), true
	case []any:
		plugins := make([]string, len(value))
		for index, item := range value {
			name, ok := item.(string)
			if !ok {
				return nil, false
			}
			plugins[index] = name
		}
		return plugins, true
	default:
		return nil, false
	}
}

func validateHTTPPluginAllowlist(names []string) error {
	seen := make(map[string]int, len(names))
	for index, name := range names {
		if name == "" {
			return fmt.Errorf("plugins[%d] %q must not be empty", index, name)
		}
		if name != strings.TrimSpace(name) {
			return fmt.Errorf("plugins[%d] %q must not have leading or trailing whitespace", index, name)
		}
		if previous, ok := seen[name]; ok {
			return fmt.Errorf("plugins[%d] %q duplicates plugins[%d]", index, name, previous)
		}
		seen[name] = index
	}
	return nil
}

func configDecodeHook(from reflect.Type, to reflect.Type, data any) (any, error) {
	if from.Kind() == reflect.String && to == reflect.TypeFor[time.Duration]() {
		return time.ParseDuration(strings.TrimSpace(data.(string)))
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
	switch value := data.(type) {
	case int:
		return value, "", true, nil
	case int8:
		return int(value), "", true, nil
	case int16:
		return int(value), "", true, nil
	case int32:
		return int(value), "", true, nil
	case int64:
		return int(value), "", true, nil
	case uint:
		return int(value), "", true, nil
	case uint8:
		return int(value), "", true, nil
	case uint16:
		return int(value), "", true, nil
	case uint32:
		return int(value), "", true, nil
	case uint64:
		return int(value), "", true, nil
	case string:
		address := strings.TrimSpace(value)
		if address == "" {
			return 0, "", true, nil
		}
		if port, err := strconv.Atoi(address); err == nil {
			return port, "", true, nil
		}
		if portString, found := strings.CutPrefix(address, ":"); found {
			port, err := strconv.Atoi(portString)
			return port, "", true, err
		}
		parsedHost, parsedPort, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			return 0, "", false, nil
		}
		port, err = strconv.Atoi(parsedPort)
		return port, parsedHost, true, err
	default:
		return 0, "", false, nil
	}
}
