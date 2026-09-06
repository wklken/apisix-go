package secret

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

type generationGCPAccount struct {
	ClientEmail string   `json:"client_email"`
	PrivateKey  string   `json:"private_key"`
	ProjectID   string   `json:"project_id"`
	TokenURI    string   `json:"token_uri"`
	Scope       []string `json:"scope"`
	EntriesURI  string   `json:"entries_uri"`
}

type generationGCPConfig struct {
	AuthConfig *generationGCPAccount `json:"auth_config"`
	AuthFile   string                `json:"auth_file"`
	SSLVerify  *bool                 `json:"ssl_verify"`
}

func (view *generationSecretView) resolveGCP(ctx context.Context, id, key string, configBytes []byte) (string, error) {
	mainKey, _, _ := strings.Cut(key, "/")
	if mainKey == "" {
		return "", ErrCredentialUnavailable
	}
	var config generationGCPConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return "", ErrCredentialUnavailable
	}
	account := config.AuthConfig
	if account == nil {
		file, err := os.Open(config.AuthFile)
		if err != nil {
			return "", ErrCredentialUnavailable
		}
		body, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
		closeErr := file.Close()
		defer clear(body)
		if err != nil || closeErr != nil || len(body) > 1<<20 {
			return "", ErrCredentialUnavailable
		}
		if err := json.Unmarshal(body, &account); err != nil {
			return "", ErrCredentialUnavailable
		}
	}
	if account == nil || account.ClientEmail == "" || account.PrivateKey == "" || account.ProjectID == "" {
		return "", ErrCredentialUnavailable
	}
	if account.TokenURI == "" {
		account.TokenURI = "https://oauth2.googleapis.com/token"
	}
	if account.EntriesURI == "" {
		account.EntriesURI = "https://secretmanager.googleapis.com/v1"
	}
	if account.Scope == nil {
		account.Scope = []string{"https://www.googleapis.com/auth/cloud-platform"}
	}
	credentials, _ := json.Marshal(account)
	defer clear(credentials)
	cacheKey := newGenerationSecretCacheKey(configBytes, string(credentials), id, key)
	if cached, ok := view.cache.get(cacheKey, time.Now()); ok {
		return cached, nil
	}
	client := view.resolver.client
	if config.SSLVerify != nil && !*config.SSLVerify {
		transport, ok := client.Transport.(*http.Transport)
		if !ok {
			return "", ErrCredentialUnavailable
		}
		cloned := transport.Clone()
		if cloned.TLSClientConfig == nil {
			cloned.TLSClientConfig = &tls.Config{}
		}
		cloned.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // APISIX ssl_verify explicitly controls this backend.
		defer cloned.CloseIdleConnections()
		copyClient := *client
		copyClient.Transport = cloned
		client = &copyClient
	}
	authorization, err := view.gcpAuthorization(ctx, client, account, credentials, id)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(
		account.EntriesURI,
		"/",
	) + "/projects/" + url.PathEscape(
		account.ProjectID,
	) + "/secrets/" + url.PathEscape(
		mainKey,
	) + "/versions/latest:access"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	body, err := readCloudSecretResponse(ctx, client, request)
	if err != nil {
		return "", err
	}
	defer clear(body)
	var response struct {
		Payload *struct {
			Data string `json:"data"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Payload == nil {
		return "", ErrCredentialUnavailable
	}
	decoded, err := base64.StdEncoding.DecodeString(response.Payload.Data)
	defer clear(decoded)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	value, err := cloudSecretField(string(decoded), key)
	if err != nil {
		return "", err
	}
	view.cache.set(cacheKey, value, generationVaultSecretCacheTTL, time.Now())
	return value, nil
}

func (view *generationSecretView) gcpAuthorization(
	ctx context.Context,
	client *http.Client,
	account *generationGCPAccount,
	credentials []byte,
	id string,
) (string, error) {
	cacheKey := newGenerationSecretCacheKey(credentials, "oauth", id, "")
	if cached, ok := view.cache.get(cacheKey, time.Now()); ok {
		return cached, nil
	}
	privateKey := []byte(account.PrivateKey)
	defer clear(privateKey)
	config := jwt.Config{
		Email:      account.ClientEmail,
		PrivateKey: privateKey,
		TokenURL:   account.TokenURI,
		Scopes:     account.Scope,
	}
	token, err := config.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client)).Token()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrCredentialUnavailable
	}
	authorization := token.Type() + " " + token.AccessToken
	ttl := 5 * time.Minute
	if !token.Expiry.IsZero() {
		ttl = min(ttl, time.Until(token.Expiry))
	}
	if ttl > 0 {
		view.cache.set(cacheKey, authorization, ttl, time.Now())
	}
	return authorization, nil
}
