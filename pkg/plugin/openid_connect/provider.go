package openid_connect

import (
	"context"
	"net/http"
	"strings"

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

// newProviderClient builds a provider from an already-fetched discovery
// document so the configured discovery URL does not have to match
// issuer + "/.well-known/openid-configuration".
func newProviderClient(
	ctx context.Context,
	doc discoveryData,
	cfg Config,
	clientSecret string,
	httpClient *http.Client,
) *providerClient {
	provider := (&oidc.ProviderConfig{
		IssuerURL:   doc.Issuer,
		AuthURL:     doc.AuthorizationEndpoint,
		TokenURL:    doc.TokenEndpoint,
		UserInfoURL: doc.UserinfoEndpoint,
		JWKSURL:     doc.JWKSURI,
		Algorithms:  doc.IDTokenSigningAlgValuesSupported,
	}).NewProvider(ctx)

	authStyle := oauth2.AuthStyleInHeader
	if cfg.TokenEndpointAuthMethod == "client_secret_post" {
		authStyle = oauth2.AuthStyleInParams
	}
	verifierConfig := &oidc.Config{ClientID: cfg.ClientID, SkipClientIDCheck: true}
	if cfg.TokenSigningAlgValuesExpected != "" {
		verifierConfig.SupportedSigningAlgs = []string{cfg.TokenSigningAlgValuesExpected}
	}

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
		verifier:      provider.VerifierContext(ctx, verifierConfig),
		userinfoURL:   doc.UserinfoEndpoint,
		introspectURL: doc.IntrospectionEndpoint,
		revokeURL:     doc.RevocationEndpoint,
	}
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
		built := newProviderClient(context.Background(), discovery, p.config, clientSecret, p.client)
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
