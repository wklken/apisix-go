package ai_auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/oauth2/jwt"

	"github.com/wklken/apisix-go/pkg/json"
)

const (
	gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	gcpJWTBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	defaultGCPMaxTTL      = 300
)

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

// NewGoogleTokenSource builds an OAuth2 token source for a Google service
// account that exchanges a signed JWT assertion for an access token.
func NewGoogleTokenSource(
	ctx context.Context,
	rawJSON []byte,
	scopes []string,
	client *http.Client,
) (oauth2.TokenSource, error) {
	var account struct {
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		TokenURI     string `json:"token_uri"`
		Subject      string `json:"subject"`
	}
	if err := json.Unmarshal(rawJSON, &account); err != nil {
		return nil, fmt.Errorf("parse Google service account: %w", err)
	}
	if account.ClientEmail == "" || account.PrivateKey == "" {
		return nil, fmt.Errorf("parse Google service account: client_email and private_key are required")
	}
	tokenURL := account.TokenURI
	if tokenURL == "" {
		tokenURL = google.JWTTokenURL
	}
	cfg := &jwt.Config{
		Email:        account.ClientEmail,
		Subject:      account.Subject,
		PrivateKey:   []byte(account.PrivateKey),
		PrivateKeyID: account.PrivateKeyID,
		TokenURL:     tokenURL,
		Scopes:       scopes,
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	return oauth2.ReuseTokenSource(nil, cfg.TokenSource(ctx)), nil
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
	token, err := s.exchangeToken(ctx, client, []byte(serviceAccountJSON), config, now)
	if err != nil {
		return "", err
	}
	s.cache[cacheKey] = cachedGCPToken{value: token.AccessToken, expires: token.Expiry}
	return token.AccessToken, nil
}

func (s *GCPTokenSource) exchangeToken(
	ctx context.Context,
	client *http.Client,
	rawJSON []byte,
	config GCPConfig,
	now time.Time,
) (*oauth2.Token, error) {
	source, err := NewGoogleTokenSource(ctx, rawJSON, []string{gcpCloudPlatformScope}, client)
	if err != nil {
		return nil, err
	}
	token, err := source.Token()
	if err != nil {
		return nil, fmt.Errorf("request GCP access token: %w", err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("invalid GCP token response")
	}
	token.Expiry = shortenedGCPTokenExpiry(token.Expiry, now, config)
	return token, nil
}

func shortenedGCPTokenExpiry(expiry time.Time, now time.Time, config GCPConfig) time.Time {
	if expiry.IsZero() {
		expiry = now.Add(time.Hour)
	}
	early := time.Duration(config.ExpireEarlySecs) * time.Second
	if config.ExpireEarlySecs == 0 {
		early = time.Minute
	}
	if expiry.Sub(now) > early {
		expiry = expiry.Add(-early)
	}
	maxTTL := config.MaxTTL
	if maxTTL == 0 {
		maxTTL = defaultGCPMaxTTL
	}
	if cap := time.Duration(maxTTL) * time.Second; expiry.Sub(now) > cap {
		expiry = now.Add(cap)
	}
	return expiry
}
