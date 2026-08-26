package openid_connect

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type discoveryData struct {
	Issuer                           string   `json:"issuer"`
	AuthorizationEndpoint            string   `json:"authorization_endpoint"`
	TokenEndpoint                    string   `json:"token_endpoint"`
	UserinfoEndpoint                 string   `json:"userinfo_endpoint"`
	EndSessionEndpoint               string   `json:"end_session_endpoint"`
	RevocationEndpoint               string   `json:"revocation_endpoint"`
	IntrospectionEndpoint            string   `json:"introspection_endpoint"`
	JWKSURI                          string   `json:"jwks_uri"`
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
}

func (p *Plugin) validateConfiguredClaims(claims map[string]any) (int, string) {
	audience, ok := p.audienceClaimValidator()
	if !ok {
		return 0, ""
	}

	value := claims[audience.claim]
	if (audience.required || audience.matchWithClientID || len(audience.validAudiences) > 0) && value == nil {
		return http.StatusForbidden, `{"error":"required audience claim not present"}`
	}
	if audience.matchWithClientID && value != nil && !audienceMatchesClientID(value, p.config.ClientID) {
		return http.StatusForbidden, `{"error":"mismatched audience"}`
	}
	if len(audience.validAudiences) > 0 && value != nil && !audienceMatches(value, audience.validAudiences) {
		return http.StatusForbidden, `{"error":"mismatched audience"}`
	}
	return 0, ""
}

type audienceClaimValidator struct {
	claim             string
	required          bool
	matchWithClientID bool
	validAudiences    []string
}

func (p *Plugin) audienceClaimValidator() (audienceClaimValidator, bool) {
	raw, ok := p.config.ClaimValidator["audience"].(map[string]any)
	if !ok {
		return audienceClaimValidator{}, false
	}

	validator := audienceClaimValidator{claim: "aud"}
	if claim, ok := raw["claim"].(string); ok && claim != "" {
		validator.claim = claim
	}
	validator.required, _ = raw["required"].(bool)
	validator.matchWithClientID, _ = raw["match_with_client_id"].(bool)
	switch values := raw["valid_audiences"].(type) {
	case []string:
		validator.validAudiences = append([]string(nil), values...)
	case []any:
		for _, value := range values {
			if audience, ok := value.(string); ok {
				validator.validAudiences = append(validator.validAudiences, audience)
			}
		}
	}
	return validator, true
}

func (p *Plugin) validateConfiguredAudiences() error {
	audience, ok := p.config.ClaimValidator["audience"].(map[string]any)
	if !ok {
		return nil
	}
	raw, exists := audience["valid_audiences"]
	if !exists {
		return nil
	}
	validate := func(value any) error {
		expected, ok := value.(string)
		if !ok {
			return errors.New("claim_validator.audience.valid_audiences must contain strings")
		}
		if expected == "" {
			return errors.New("claim_validator.audience.valid_audiences must not contain empty strings")
		}
		return nil
	}
	switch values := raw.(type) {
	case []string:
		for _, value := range values {
			if err := validate(value); err != nil {
				return err
			}
		}
	case []any:
		for _, value := range values {
			if err := validate(value); err != nil {
				return err
			}
		}
	default:
		return errors.New("claim_validator.audience.valid_audiences must be an array")
	}
	return nil
}

func audienceMatchesClientID(value any, clientID string) bool {
	return audienceMatches(value, []string{clientID})
}

func audienceMatches(value any, expected []string) bool {
	switch typed := value.(type) {
	case string:
		return slices.Contains(expected, typed)
	case []any:
		if slices.ContainsFunc(typed, func(item any) bool {
			audience, ok := item.(string)
			return ok && slices.Contains(expected, audience)
		}) {
			return true
		}
	case []string:
		if slices.ContainsFunc(typed, func(audience string) bool { return slices.Contains(expected, audience) }) {
			return true
		}
	}
	return false
}

func (p *Plugin) localJWTAudienceValid(claims map[string]any) bool {
	validator, configured := p.audienceClaimValidator()
	if !configured {
		return audienceMatchesClientID(claims["aud"], p.config.ClientID)
	}

	value := claims[validator.claim]
	constrained := false
	if validator.matchWithClientID {
		constrained = true
		if !audienceMatchesClientID(value, p.config.ClientID) {
			return false
		}
	}
	if len(validator.validAudiences) > 0 {
		constrained = true
		if !audienceMatches(value, validator.validAudiences) {
			return false
		}
	}
	if !constrained {
		return audienceMatchesClientID(claims["aud"], p.config.ClientID)
	}
	return true
}

func (p *Plugin) validateClaimSchema(claims map[string]any) error {
	return p.validateSchema(claims)
}

func (p *Plugin) validateSessionClaimSchema(tokens tokenResponse, userinfo string) error {
	if len(p.config.ClaimSchema) == 0 {
		return nil
	}
	var user any
	if userinfo != "" {
		if err := json.Unmarshal([]byte(userinfo), &user); err != nil {
			return fmt.Errorf("invalid userinfo response")
		}
	}
	var idToken any = tokens.IDToken
	if tokens.IDToken != "" {
		if token, err := base.ParseJWT(tokens.IDToken); err == nil {
			idToken = token.Payload
		}
	}
	return p.validateSchema(map[string]any{
		"user":         user,
		"access_token": tokens.AccessToken,
		"id_token":     idToken,
	})
}

func (p *Plugin) validateSchema(value any) error {
	if p.claimSchema == nil {
		return nil
	}
	if err := p.claimSchema.Validate(value); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) bearerToken(r *http.Request, clientXAccessToken string) (bool, string, int, string) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		if clientXAccessToken == "" {
			return false, "", 0, ""
		}
		return true, clientXAccessToken, 0, ""
	}

	parts := strings.SplitN(auth, " ", 2)
	if len(parts) < 2 {
		return false, "", http.StatusBadRequest, "Invalid Authorization header format."
	}
	if strings.EqualFold(parts[0], "Bearer") {
		return true, parts[1], 0, ""
	}
	return false, "", 0, ""
}

func (p *Plugin) usesLocalJWTVerification() bool {
	return p.config.PublicKey != "" || p.config.UseJWKS
}

func (p *Plugin) verifyPresentIDToken(r *http.Request, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	claims, err := p.verifyBearerJWT(r, rawToken)
	if err != nil {
		return err
	}
	if !locallyVerifiedTokenActive(claims) {
		return fmt.Errorf("JWT token claims invalid")
	}
	return nil
}

func (p *Plugin) verifyBearerJWT(r *http.Request, rawToken string) (map[string]any, error) {
	token, err := base.ParseJWT(rawToken)
	if err != nil {
		return nil, fmt.Errorf("JWT token invalid")
	}
	algorithm, _ := token.Header["alg"].(string)
	if algorithm == "" {
		return nil, fmt.Errorf("JWT token missing alg")
	}
	if !validTokenSigningAlgorithm(algorithm) {
		return nil, fmt.Errorf("JWT token alg unsupported")
	}
	if p.config.TokenSigningAlgValuesExpected != "" && algorithm != p.config.TokenSigningAlgValuesExpected {
		return nil, fmt.Errorf("JWT token alg mismatch")
	}

	var idToken *oidc.IDToken
	if p.config.PublicKey != "" {
		err = p.withOIDCPublicKey(func(publicKey crypto.PublicKey) error {
			verifier := p.staticKeyVerifier(algorithm, publicKey)
			idToken, err = verifier.Verify(r.Context(), rawToken)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to verify jwt")
		}
	} else {
		client, err := p.providerClient(r)
		if err != nil {
			return nil, fmt.Errorf("failed to verify jwt")
		}
		idToken, err = client.verifier.Verify(r.Context(), rawToken)
		if err != nil {
			return nil, fmt.Errorf("failed to verify jwt")
		}
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("failed to parse jwt claims")
	}
	p.validateIssuer(claims)
	if !p.localJWTAudienceValid(claims) {
		claims["active"] = false
	}

	return claims, nil
}

// staticKeyVerifier builds a go-oidc verifier over the configured static
// public key. Issuer validation is deferred to validateIssuer because the
// plugin accepts an explicit issuer list.
func (p *Plugin) staticKeyVerifier(algorithm string, publicKey crypto.PublicKey) *oidc.IDTokenVerifier {
	return oidc.NewVerifier("", &oidc.StaticKeySet{
		PublicKeys: []crypto.PublicKey{publicKey},
	}, &oidc.Config{
		SkipClientIDCheck:    true,
		SkipIssuerCheck:      true,
		SupportedSigningAlgs: []string{algorithm},
	})
}

func parsePublicKey(publicKeyBytes []byte) (any, error) {
	if block, _ := pem.Decode(publicKeyBytes); block != nil {
		publicKeyBytes = block.Bytes
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(publicKeyBytes)
			if err != nil {
				return nil, err
			}
			return cert.PublicKey, nil
		}
	}

	if publicKey, err := x509.ParsePKIXPublicKey(publicKeyBytes); err == nil {
		return publicKey, nil
	}
	if publicKey, err := x509.ParsePKCS1PublicKey(publicKeyBytes); err == nil {
		return publicKey, nil
	}
	return nil, fmt.Errorf("unsupported public key")
}

func (p *Plugin) validateIssuer(payload map[string]any) {
	issuer, _ := payload["iss"].(string)
	configured, _ := p.configuredIssuers()
	if len(configured) > 0 {
		if issuer == "" || !slices.Contains(configured, issuer) {
			payload["active"] = false
		}
		return
	}
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.Issuer == "" {
		payload["active"] = false
		return
	}
	if issuer == "" || issuer != discovery.Issuer {
		payload["active"] = false
	}
}

func (p *Plugin) configuredIssuers() ([]string, error) {
	issuer, ok := p.config.ClaimValidator["issuer"].(map[string]any)
	if !ok {
		return nil, nil
	}
	raw, exists := issuer["valid_issuers"]
	if !exists {
		return nil, nil
	}
	switch values := raw.(type) {
	case []string:
		return append([]string(nil), values...), nil
	case []any:
		issuers := make([]string, 0, len(values))
		for _, value := range values {
			issuer, ok := value.(string)
			if !ok {
				return nil, errors.New("claim_validator.issuer.valid_issuers must contain strings")
			}
			issuers = append(issuers, issuer)
		}
		return issuers, nil
	default:
		return nil, errors.New("claim_validator.issuer.valid_issuers must be an array")
	}
}

func (p *Plugin) introspect(r *http.Request, token string) (map[string]any, error) {
	endpoint, err := p.introspectionEndpoint()
	if err != nil {
		return nil, err
	}

	var claims map[string]any
	err = p.authenticatedFormRequest(
		r,
		endpoint,
		func(form url.Values) { form.Set("token", token) },
		p.config.IntrospectionEndpointAuthMethod,
		func(req *http.Request) error {
			for _, name := range p.config.IntrospectionAddonHeaders {
				if value := r.Header.Get(name); value != "" {
					req.Header.Set(name, value)
				}
			}
			resp, err := p.client.Do(req)
			if err != nil {
				return err
			}
			defer func() { _ = resp.Body.Close() }()
			clearOIDCRequestCredentials(resp.Request)
			if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				return fmt.Errorf("introspection endpoint returned %d", resp.StatusCode)
			}
			return json.NewDecoder(resp.Body).Decode(&claims)
		},
	)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (p *Plugin) introspectionEndpoint() (string, error) {
	if p.config.IntrospectionEndpoint != "" {
		return p.config.IntrospectionEndpoint, nil
	}
	discovery, err := p.discoveryDoc()
	if err != nil {
		return "", err
	}
	if discovery.IntrospectionEndpoint == "" {
		return "", errors.New("openid discovery document has no introspection_endpoint")
	}
	return discovery.IntrospectionEndpoint, nil
}

func (p *Plugin) discoveryDoc() (discoveryData, error) {
	p.mu.Lock()
	if p.discoveryLoaded {
		discovery := p.discovery
		p.mu.Unlock()
		return discovery, nil
	}
	p.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, p.config.Discovery, nil)
	if err != nil {
		return discoveryData{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return discoveryData{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return discoveryData{}, fmt.Errorf("discovery endpoint returned %d", resp.StatusCode)
	}

	var discovery discoveryData
	if err := json.NewDecoder(resp.Body).Decode(&discovery); err != nil {
		return discoveryData{}, err
	}

	p.mu.Lock()
	p.discovery = discovery
	p.discoveryLoaded = true
	p.mu.Unlock()

	return discovery, nil
}

func (p *Plugin) setAccessTokenHeader(r *http.Request, token string) {
	if !*p.config.SetAccessTokenHeader {
		return
	}
	if p.config.AccessTokenInAuthorizationHeader {
		r.Header.Set("Authorization", "Bearer "+token)
		return
	}
	r.Header.Set("X-Access-Token", token)
}

func (p *Plugin) writeBearerUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, p.config.Realm))
	http.Error(w, message, http.StatusUnauthorized)
}

func (p *Plugin) writeInvalidToken(w http.ResponseWriter, message string) {
	w.Header().
		Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s", error="invalid_token", error_description="%s"`, p.config.Realm, message))
	http.Error(w, message, http.StatusUnauthorized)
}
