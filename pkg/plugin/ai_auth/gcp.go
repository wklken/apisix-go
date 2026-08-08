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
	mu       sync.Mutex
	cache    map[string]cachedGCPToken
	inflight map[string]*gcpTokenRefresh
	now      func() time.Time
}

type cachedGCPToken struct {
	value   string
	expires time.Time
}

// gcpTokenRefresh coordinates one in-flight token exchange so concurrent
// callers wait for a single refresh instead of serializing network I/O under
// the cache lock.
type gcpTokenRefresh struct {
	done  chan struct{}
	value string
	err   error
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
	return &GCPTokenSource{
		cache:    make(map[string]cachedGCPToken),
		inflight: make(map[string]*gcpTokenRefresh),
		now:      time.Now,
	}
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
	now := s.now()
	if cached, ok := s.cache[cacheKey]; ok && now.Before(cached.expires) {
		s.mu.Unlock()
		return cached.value, nil
	}
	if refresh, ok := s.inflight[cacheKey]; ok {
		s.mu.Unlock()
		select {
		case <-refresh.done:
			if refresh.err != nil {
				return "", refresh.err
			}
			return refresh.value, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	refresh := &gcpTokenRefresh{done: make(chan struct{})}
	s.inflight[cacheKey] = refresh
	s.mu.Unlock()

	// The token exchange performs network I/O and must never run while the
	// cache lock is held.
	token, err := s.exchangeToken(ctx, client, []byte(serviceAccountJSON), config, now)

	s.mu.Lock()
	switch err {
	case nil:
		cached := cachedGCPToken{value: token.AccessToken, expires: token.Expiry}
		s.cache[cacheKey] = cached
		refresh.value = cached.value
	default:
		// Keep the previous valid token until its expiry; refresh errors
		// only surface once even that token is stale.
		if cached, ok := s.cache[cacheKey]; ok && s.now().Before(cached.expires) {
			refresh.value = cached.value
		} else {
			refresh.err = err
		}
	}
	delete(s.inflight, cacheKey)
	s.mu.Unlock()
	close(refresh.done)

	if refresh.err != nil {
		return "", refresh.err
	}
	return refresh.value, nil
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
