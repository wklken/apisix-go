package openid_connect

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// providerClient is the go-oidc/oauth2 view of a fetched discovery document.
type providerClient struct {
	oauth2Config  oauth2.Config
	verifier      *oidc.IDTokenVerifier
	userinfoURL   string
	introspectURL string
	revokeURL     string
}

type expiringRemoteKeySet struct {
	mu         sync.Mutex
	ctx        context.Context
	url        string
	httpClient *http.Client
	expiresIn  time.Duration
	now        func() time.Time
	keySet     *oidc.RemoteKeySet
	expiresAt  time.Time
}

func (s *expiringRemoteKeySet) newKeySet() *oidc.RemoteKeySet {
	ctx := s.ctx
	if s.httpClient != nil {
		ctx = oidc.ClientContext(ctx, s.httpClient)
	}
	return oidc.NewRemoteKeySet(ctx, s.url)
}

func (s *expiringRemoteKeySet) VerifySignature(ctx context.Context, rawToken string) ([]byte, error) {
	now := s.now()
	s.mu.Lock()
	keySet := s.keySet
	if keySet == nil || s.expiresIn <= 0 || !now.Before(s.expiresAt) {
		keySet = s.newKeySet()
	}
	s.mu.Unlock()

	if s.httpClient != nil {
		ctx = oidc.ClientContext(ctx, s.httpClient)
	}
	payload, err := keySet.VerifySignature(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	if s.expiresIn > 0 {
		s.mu.Lock()
		if s.keySet == nil || !now.Before(s.expiresAt) || s.keySet != keySet {
			s.keySet = keySet
			s.expiresAt = now.Add(s.expiresIn)
		}
		s.mu.Unlock()
	}
	return payload, nil
}

// newProviderClient builds a provider from an already-fetched discovery
// document so the configured discovery URL does not have to match
// issuer + "/.well-known/openid-configuration".
func newProviderClient(
	ctx context.Context,
	doc discoveryData,
	cfg Config,
	clientSecret string,
	httpClient *http.Client,
	now func() time.Time,
) *providerClient {
	authStyle := oauth2.AuthStyleInHeader
	if cfg.TokenEndpointAuthMethod == "client_secret_post" {
		authStyle = oauth2.AuthStyleInParams
	}
	verifierConfig := &oidc.Config{ClientID: cfg.ClientID, SkipClientIDCheck: true}
	if cfg.TokenSigningAlgValuesExpected != "" {
		verifierConfig.SupportedSigningAlgs = []string{cfg.TokenSigningAlgValuesExpected}
	}
	verifierConfig.SkipExpiryCheck = true
	if now != nil {
		verifierConfig.Now = now
	}
	if len(verifierConfig.SupportedSigningAlgs) == 0 {
		verifierConfig.SupportedSigningAlgs = append(
			[]string(nil),
			doc.IDTokenSigningAlgValuesSupported...,
		)
	}
	var keySet oidc.KeySet = &oidc.StaticKeySet{}
	if doc.JWKSURI != "" {
		if now == nil {
			now = time.Now
		}
		keySet = &expiringRemoteKeySet{
			ctx:        ctx,
			url:        doc.JWKSURI,
			httpClient: httpClient,
			expiresIn:  time.Duration(pointerIntValue(cfg.JWKExpiresIn, 86400)) * time.Second,
			now:        now,
		}
	}
	verifier := oidc.NewVerifier(doc.Issuer, keySet, verifierConfig)

	return &providerClient{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: clientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   doc.AuthorizationEndpoint,
				TokenURL:  doc.TokenEndpoint,
				AuthStyle: authStyle,
			},
			Scopes: strings.Fields(cfg.Scope),
		},
		verifier:      verifier,
		userinfoURL:   doc.UserinfoEndpoint,
		introspectURL: doc.IntrospectionEndpoint,
		revokeURL:     doc.RevocationEndpoint,
	}
}

func pointerIntValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func (p *Plugin) providerClient(r *http.Request) (*providerClient, error) {
	releaseWork, err := p.acquireReadyOIDCWork()
	if err != nil {
		return nil, err
	}
	defer releaseWork()
	p.providerMu.Lock()
	if p.provider != nil {
		if p.retired.Load() {
			p.providerMu.Unlock()
			return nil, errOIDCCredentialsUnavailable
		}
		client := p.provider
		p.providerMu.Unlock()
		return client, nil
	}
	p.providerMu.Unlock()

	discovery, err := p.discoveryDoc()
	if err != nil {
		return nil, err
	}
	// The provider is built once per plugin instance so the verifier keeps
	// its JWKS remote-key-set cache instead of refetching keys per request.
	var client *providerClient
	err = p.withClientSecret(func(clientSecret string) error {
		built := newProviderClient(context.Background(), discovery, p.config, clientSecret, p.client, p.currentTime)
		p.providerMu.Lock()
		if p.provider == nil {
			p.provider = built
		}
		client = p.provider
		p.providerMu.Unlock()
		return nil
	})
	return client, err
}
