package secret

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/wklken/apisix-go/pkg/json"
)

type generationAWSConfig struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	Region          string `json:"region"`
	EndpointURL     string `json:"endpoint_url"`
}

func (view *generationSecretView) resolveAWS(ctx context.Context, id, key string, configBytes []byte) (string, error) {
	mainKey, _, _ := strings.Cut(key, "/")
	if mainKey == "" {
		return "", ErrCredentialUnavailable
	}
	var config generationAWSConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return "", ErrCredentialUnavailable
	}
	for _, value := range []*string{&config.AccessKeyID, &config.SecretAccessKey, &config.SessionToken} {
		resolved, err := backendEnvironment(ctx, *value)
		if err != nil {
			return "", err
		}
		*value = resolved
	}
	if config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return "", ErrCredentialUnavailable
	}
	if config.Region == "" {
		config.Region = "us-east-1"
	}
	if config.EndpointURL == "" {
		config.EndpointURL = "https://secretsmanager." + config.Region + ".amazonaws.com"
	}
	credentials, _ := json.Marshal(config)
	defer clear(credentials)
	cacheKey := newGenerationSecretCacheKey(credentials, "", id, key)
	if cached, ok := view.cache.get(cacheKey, time.Now()); ok {
		return cached, nil
	}
	endpoint, err := url.Parse(config.EndpointURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", ErrCredentialUnavailable
	}
	endpoint.Path, endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "/", "", "", ""
	payload, _ := json.Marshal(map[string]string{"SecretId": mainKey, "VersionStage": "AWSCURRENT"})
	defer clear(payload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	request.Header.Set("Content-Type", "application/x-amz-json-1.1")
	request.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	digest := sha256.Sum256(payload)
	err = v4.NewSigner().
		SignHTTP(ctx, aws.Credentials{AccessKeyID: config.AccessKeyID, SecretAccessKey: config.SecretAccessKey, SessionToken: config.SessionToken}, request, hex.EncodeToString(digest[:]), "secretsmanager", config.Region, time.Now())
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	body, err := readCloudSecretResponse(ctx, view.resolver.client, request)
	if err != nil {
		return "", err
	}
	defer clear(body)
	var response struct {
		SecretString *string `json:"SecretString"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.SecretString == nil {
		return "", ErrCredentialUnavailable
	}
	value, err := cloudSecretField(*response.SecretString, key)
	if err != nil {
		return "", err
	}
	view.cache.set(cacheKey, value, generationVaultSecretCacheTTL, time.Now())
	return value, nil
}
