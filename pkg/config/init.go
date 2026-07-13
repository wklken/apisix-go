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
	var cfg Config
	err := v.Unmarshal(&cfg, viper.DecodeHook(configDecodeHook))
	if err != nil {
		return nil, err
	}

	GlobalConfig = &cfg
	data_encryption.Configure(cfg.Apisix.DataEncryption.EnableEncryptFields, cfg.Apisix.DataEncryption.Keyring)

	return &cfg, nil
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
