package ai_auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

const gcpJWTBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

type GCPConfig struct {
	ServiceAccountJSON string `json:"service_account_json,omitempty"`
	MaxTTL             int    `json:"max_ttl,omitempty"`
	ExpireEarlySecs    int    `json:"expire_early_secs,omitempty"`
}

type GCPTokenSource struct {
	mu    sync.Mutex
	cache map[string]cachedGCPToken
	now   func() time.Time
}

type cachedGCPToken struct {
	value   string
	expires time.Time
}

type gcpServiceAccount struct {
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
	TokenURI     string `json:"token_uri"`
}

type gcpTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

func NewGCPTokenSource() *GCPTokenSource {
	return &GCPTokenSource{cache: make(map[string]cachedGCPToken), now: time.Now}
}

func (s *GCPTokenSource) Apply(
	ctx context.Context,
	client *http.Client,
	req *http.Request,
	config GCPConfig,
) error {
	token, err := s.Token(ctx, client, config)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (s *GCPTokenSource) Token(ctx context.Context, client *http.Client, config GCPConfig) (string, error) {
	serviceAccountJSON := config.ServiceAccountJSON
	if serviceAccountJSON == "" {
		serviceAccountJSON = os.Getenv("GCP_SERVICE_ACCOUNT")
	}
	if serviceAccountJSON == "" {
		return "", fmt.Errorf("GCP service_account_json or GCP_SERVICE_ACCOUNT is required")
	}
	var account gcpServiceAccount
	if err := json.Unmarshal([]byte(serviceAccountJSON), &account); err != nil {
		return "", fmt.Errorf("invalid GCP service account JSON: %w", err)
	}
	if account.ClientEmail == "" || account.PrivateKey == "" || account.TokenURI == "" {
		return "", fmt.Errorf("GCP service account requires client_email, private_key, and token_uri")
	}
	cacheKey := sha256Hex([]byte(serviceAccountJSON))

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if cached, ok := s.cache[cacheKey]; ok && now.Before(cached.expires) {
		return cached.value, nil
	}
	assertion, err := buildGCPJWTAssertion(account, now)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {gcpJWTBearerGrantType},
		"assertion":  {assertion},
	}
	tokenReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		account.TokenURI,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("create GCP token request: %w", err)
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("request GCP access token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read GCP token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GCP token endpoint returned status %d", resp.StatusCode)
	}
	var token gcpTokenResponse
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" {
		return "", fmt.Errorf("invalid GCP token response")
	}
	ttl := token.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	early := config.ExpireEarlySecs
	if early == 0 {
		early = 60
	}
	if ttl > early {
		ttl -= early
	}
	if config.MaxTTL > 0 && ttl > config.MaxTTL {
		ttl = config.MaxTTL
	}
	s.cache[cacheKey] = cachedGCPToken{value: token.AccessToken, expires: now.Add(time.Duration(ttl) * time.Second)}
	return token.AccessToken, nil
}

func buildGCPJWTAssertion(account gcpServiceAccount, now time.Time) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	if account.PrivateKeyID != "" {
		header["kid"] = account.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   account.ClientEmail,
		"scope": "https://www.googleapis.com/auth/cloud-platform",
		"aud":   account.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	encodedHeader, err := encodeGCPJWTPart(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeGCPJWTPart(claims)
	if err != nil {
		return "", err
	}
	unsigned := encodedHeader + "." + encodedClaims
	key, err := parseGCPPrivateKey(account.PrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GCP JWT: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func encodeGCPJWTPart(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode GCP JWT: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func parseGCPPrivateKey(value string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, fmt.Errorf("invalid GCP private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GCP private key: %w", err)
	}
	return key, nil
}
