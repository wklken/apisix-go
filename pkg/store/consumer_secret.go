package store

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/resource"
)

const (
	environmentSecretPrefix = "$ENV://"
	managedSecretPrefix     = "$secret://"
)

type vaultSecretConfig struct {
	URI       string `json:"uri"`
	Prefix    string `json:"prefix"`
	Token     string `json:"token"`
	Namespace string `json:"namespace,omitempty"`
	Timeout   int    `json:"timeout,omitempty"`
}

func (s *Store) resolveConsumerSecretValue(value any) (any, error) {
	switch typed := value.(type) {
	case string:
		return s.resolveConsumerSecretString(typed)
	case map[string]any:
		resolvedMap := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := s.resolveConsumerSecretValue(item)
			if err != nil {
				return nil, err
			}
			resolvedMap[key] = resolved
		}
		return resolvedMap, nil
	case []any:
		resolvedItems := make([]any, len(typed))
		for index, item := range typed {
			resolved, err := s.resolveConsumerSecretValue(item)
			if err != nil {
				return nil, err
			}
			resolvedItems[index] = resolved
		}
		return resolvedItems, nil
	default:
		return value, nil
	}
}

func (s *Store) resolveConsumerPlugin(consumer resource.Consumer, pluginName string) (resource.Consumer, error) {
	config, ok := consumer.Plugins[pluginName]
	if !ok {
		return resource.Consumer{}, fmt.Errorf("consumer plugin %q not found", pluginName)
	}
	resolved, err := s.resolveConsumerSecretValue(config)
	if err != nil {
		return resource.Consumer{}, err
	}
	plugins := make(map[string]resource.PluginConfig, len(consumer.Plugins))
	for name, pluginConfig := range consumer.Plugins {
		plugins[name] = cloneConsumerValue(pluginConfig)
	}
	plugins[pluginName] = resolved
	consumer.Plugins = plugins
	if consumer.Labels != nil {
		consumer.Labels = cloneConsumerValue(consumer.Labels).(map[string]any)
	}
	return consumer, nil
}

func cloneConsumerValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneConsumerValue(item)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneConsumerValue(item)
		}
		return cloned
	default:
		return value
	}
}

func (s *Store) resolveConsumerSecretString(value string) (string, error) {
	if len(value) >= len(environmentSecretPrefix) &&
		strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix) {
		return resolveEnvironmentSecret(value)
	}
	if strings.HasPrefix(value, managedSecretPrefix) {
		return s.resolveManagedSecret(value)
	}
	return value, nil
}

func resolveEnvironmentSecret(reference string) (string, error) {
	pathParts := strings.Split(reference[len(environmentSecretPrefix):], "/")
	name := pathParts[0]
	if name == "" {
		return "", fmt.Errorf("environment secret name is empty")
	}
	value, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("environment secret %q is not set", name)
	}
	if len(pathParts) == 1 {
		return value, nil
	}
	var document any
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return "", fmt.Errorf("decode environment secret %s: %w", name, err)
	}
	current := document
	for _, key := range pathParts[1:] {
		object, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("environment secret %q path is not an object", name)
		}
		current, ok = object[key]
		if !ok {
			return "", fmt.Errorf("environment secret %q path %q is not set", name, key)
		}
	}
	resolved, ok := current.(string)
	if !ok {
		return "", fmt.Errorf("environment secret %q resolved value is not a string", name)
	}
	return resolved, nil
}

func (s *Store) resolveManagedSecret(reference string) (string, error) {
	parts := strings.SplitN(strings.TrimPrefix(reference, managedSecretPrefix), "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("invalid managed secret reference %q", reference)
	}
	if parts[0] != "vault" {
		return "", fmt.Errorf("unsupported secret manager %q", parts[0])
	}
	return s.resolveVaultSecret(parts[0]+"/"+parts[1], parts[2])
}

func (s *Store) resolveVaultSecret(id, key string) (string, error) {
	raw := s.GetFromBucket("secrets", []byte(id))
	if raw == nil {
		return "", fmt.Errorf("secret resource %q not found", id)
	}
	var config vaultSecretConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("decode Vault secret resource %q: %w", id, err)
	}
	if config.URI == "" || config.Prefix == "" || config.Token == "" {
		return "", fmt.Errorf("vault secret resource %q requires uri, prefix, and token", id)
	}
	lastSlash := strings.LastIndexByte(key, '/')
	if lastSlash <= 0 || lastSlash == len(key)-1 {
		return "", fmt.Errorf("vault secret key %q requires a path and field", key)
	}
	token, err := resolveEnvironmentSecretReference(config.Token)
	if err != nil {
		return "", fmt.Errorf("resolve Vault token: %w", err)
	}
	endpoint, err := url.Parse(strings.TrimRight(config.URI, "/"))
	if err != nil {
		return "", fmt.Errorf("parse Vault URI: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return "", fmt.Errorf("vault URI scheme must be http or https")
	}
	endpoint.Path = path.Join(endpoint.Path, "/v1", config.Prefix, key[:lastSlash])
	request, err := http.NewRequest(http.MethodGet, endpoint.String(), bytes.NewBufferString("{}"))
	if err != nil {
		return "", fmt.Errorf("create Vault request: %w", err)
	}
	request.Header.Set("X-Vault-Token", token)
	if config.Namespace != "" {
		request.Header.Set("X-Vault-Namespace", config.Namespace)
	}
	timeout := time.Duration(config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return "", fmt.Errorf("request Vault secret: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil {
		return "", fmt.Errorf("read Vault response: %w", err)
	}
	if len(body) > 1<<20 {
		return "", fmt.Errorf("vault response exceeds 1 MiB")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("vault response status %d", response.StatusCode)
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode Vault response: %w", err)
	}
	value, ok := payload.Data[key[lastSlash+1:]].(string)
	if !ok {
		return "", fmt.Errorf("vault response does not contain string field %q", key[lastSlash+1:])
	}
	return value, nil
}

func resolveEnvironmentSecretReference(value string) (string, error) {
	if len(value) < len(environmentSecretPrefix) ||
		!strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix) {
		return value, nil
	}
	resolved, err := resolveEnvironmentSecret(value)
	if err != nil {
		return "", err
	}
	return resolved, nil
}
