package openid_connect

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"golang.org/x/oauth2"
)

func (p *Plugin) handleCodeFlow(w http.ResponseWriter, r *http.Request, next http.Handler) {
	redirectURI := p.redirectURI(r)
	if p.isRedirectCallback(r, redirectURI) {
		p.handleCodeCallback(w, r, redirectURI)
		return
	}
	if p.config.ForceReauthorize {
		p.beginAuthorization(w, r, redirectURI, nil, "")
		return
	}

	session, err := p.readSession(r)
	now := p.currentTime()
	if err == nil && session != nil && p.sessionValid(*session, now) {
		if p.refreshSessionDue(*session, now) {
			p.beginAuthorization(w, r, redirectURI, session, "none")
			return
		}
		session.UpdatedAt = now.Unix()
		if p.config.Session.RollingTimeout > 0 || p.config.Session.IdlingTimeout > 0 {
			if err := p.writeSession(w, *session); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
		}
		p.setSessionHeaders(r, *session)
		next.ServeHTTP(w, r)
		return
	}
	if err == nil && session != nil && *p.config.RenewAccessTokenOnExpiry && p.sessionRefreshable(*session, now) {
		tokens, err := p.refreshAccessToken(r, session.RefreshToken)
		if err == nil {
			session.UpdatedAt = now.Unix()
			session.AccessToken = tokens.AccessToken
			if tokens.IDToken != "" {
				session.IDToken = tokens.IDToken
			}
			if tokens.RefreshToken != "" {
				session.RefreshToken = tokens.RefreshToken
			}
			session.ExpiresAt = p.tokenExpiresAt(now, tokens.ExpiresIn)
			if err := p.writeSession(w, *session); err != nil {
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			p.setSessionHeaders(r, *session)
			next.ServeHTTP(w, r)
			return
		}
	}

	p.beginAuthorization(w, r, redirectURI, nil, "")
}

func (p *Plugin) beginAuthorization(
	w http.ResponseWriter,
	r *http.Request,
	redirectURI string,
	previous *sessionData,
	prompt string,
) {
	client, err := p.providerClient(r)
	if err != nil || client.oauth2Config.Endpoint.AuthURL == "" {
		http.Error(w, "openid discovery document has no authorization_endpoint", http.StatusBadGateway)
		return
	}

	state, err := randomURLValue(32)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	now := p.currentTime()
	session := sessionData{
		CreatedAt:     now.Unix(),
		UpdatedAt:     now.Unix(),
		FlowState:     state,
		FlowExpiresAt: p.flowExpiry(now).Unix(),
		OriginalURI:   r.URL.RequestURI(),
	}
	if previous != nil {
		session.CreatedAt = previous.CreatedAt
		session.RedisID = previous.RedisID
	}

	options := make([]oauth2.AuthCodeOption, 0, len(p.config.AuthorizationParams)+2)
	for key, value := range p.config.AuthorizationParams {
		if value != nil {
			options = append(options, oauth2.SetAuthURLParam(key, fmt.Sprint(value)))
		}
	}
	if prompt != "" {
		options = append(options, oauth2.SetAuthURLParam("prompt", prompt))
	}
	if p.config.UsePKCE {
		verifier, err := randomURLValue(32)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		session.CodeVerifier = verifier
		options = append(options, oauth2.S256ChallengeOption(verifier))
	}
	if err := p.writeSession(w, session); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	oauth2Config := client.oauth2Config
	oauth2Config.RedirectURL = redirectURI
	authorizationURL := oauth2Config.AuthCodeURL(state, options...)
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

func (p *Plugin) handleCodeCallback(w http.ResponseWriter, r *http.Request, redirectURI string) {
	apisixctx.RegisterSensitiveQueryName(r, "code")
	session, err := p.readSession(r)
	state := r.URL.Query().Get("state")
	if err != nil || session == nil || state == "" || session.FlowState == "" ||
		session.FlowExpiresAt <= p.currentTime().Unix() ||
		subtle.ConstantTimeCompare([]byte(state), []byte(session.FlowState)) != 1 {
		http.Error(w, "invalid authorization state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	tokens, err := p.exchangeCode(r, code, redirectURI, session.CodeVerifier)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	now := p.currentTime()
	newSession := sessionData{
		RedisID:           session.RedisID,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         now.Unix(),
		LastAuthenticated: now.Unix(),
		AccessToken:       tokens.AccessToken,
		IDToken:           tokens.IDToken,
		RefreshToken:      tokens.RefreshToken,
	}
	newSession.ExpiresAt = p.tokenExpiresAt(now, tokens.ExpiresIn)
	if userinfo, err := p.userinfo(r, tokens.AccessToken); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	} else {
		newSession.Userinfo = userinfo
	}
	if err := p.validateSessionClaimSchema(tokens, newSession.Userinfo); err != nil {
		p.writeInvalidToken(w, err.Error())
		return
	}
	if err := p.writeSession(w, newSession); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	originalURI := safeOriginalURI(session.OriginalURI)
	http.Redirect(w, r, originalURI, http.StatusFound)
}

func safeOriginalURI(originalURI string) string {
	if originalURI == "" || !strings.HasPrefix(originalURI, "/") ||
		strings.HasPrefix(originalURI, "//") || strings.Contains(originalURI, "\\") ||
		strings.IndexFunc(originalURI, unicode.IsControl) >= 0 {
		return "/"
	}

	parsed, err := url.ParseRequestURI(originalURI)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path == "" ||
		strings.HasPrefix(parsed.Path, "//") || strings.Contains(parsed.Path, "\\") ||
		strings.IndexFunc(parsed.Path, unicode.IsControl) >= 0 {
		return "/"
	}
	return originalURI
}

func (p *Plugin) exchangeCode(r *http.Request, code, redirectURI, verifier string) (tokenResponse, error) {
	if standardClientAuth(p.config.TokenEndpointAuthMethod) {
		client, err := p.providerClient(r)
		if err != nil {
			return tokenResponse{}, err
		}
		oauth2Config := client.oauth2Config
		oauth2Config.RedirectURL = redirectURI
		options := []oauth2.AuthCodeOption{}
		if p.config.UsePKCE {
			if verifier == "" {
				return tokenResponse{}, errors.New("missing PKCE verifier")
			}
			options = append(options, oauth2.VerifierOption(verifier))
		}
		token, err := oauth2Config.Exchange(r.Context(), code, options...)
		if err != nil {
			return tokenResponse{}, err
		}
		return p.tokenResponseFromOAuth2(token), nil
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if p.config.UsePKCE {
		if verifier == "" {
			return tokenResponse{}, errors.New("missing PKCE verifier")
		}
		form.Set("code_verifier", verifier)
	}
	return p.requestTokens(r, form)
}

func (p *Plugin) refreshAccessToken(r *http.Request, refreshToken string) (tokenResponse, error) {
	if standardClientAuth(p.config.TokenEndpointAuthMethod) {
		client, err := p.providerClient(r)
		if err != nil {
			return tokenResponse{}, err
		}
		token, err := client.oauth2Config.TokenSource(
			r.Context(),
			&oauth2.Token{RefreshToken: refreshToken},
		).Token()
		if err != nil {
			return tokenResponse{}, err
		}
		return p.tokenResponseFromOAuth2(token), nil
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", p.config.Scope)
	return p.requestTokens(r, form)
}

// standardClientAuth reports whether x/oauth2 can express the configured
// token-endpoint authentication; JWT assertion methods stay on the custom
// request builder.
func standardClientAuth(method string) bool {
	return method == "" || method == "client_secret_basic" || method == "client_secret_post"
}

func (p *Plugin) requestTokens(r *http.Request, form url.Values) (tokenResponse, error) {
	discovery, err := p.discoveryDoc()
	if err != nil {
		return tokenResponse{}, err
	}
	if discovery.TokenEndpoint == "" {
		return tokenResponse{}, errors.New("openid discovery document has no token_endpoint")
	}
	resp, err := p.postTokenForm(r, discovery.TokenEndpoint, form)
	if err != nil {
		return tokenResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return tokenResponse{}, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokens tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return tokenResponse{}, fmt.Errorf("invalid token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return tokenResponse{}, errors.New("token response has no access_token")
	}
	return tokens, nil
}

func (p *Plugin) postTokenForm(r *http.Request, endpoint string, form url.Values) (*http.Response, error) {
	req, err := p.authenticatedFormRequest(r, endpoint, form, p.config.TokenEndpointAuthMethod)
	if err != nil {
		return nil, err
	}
	return p.client.Do(req)
}

func (p *Plugin) authenticatedFormRequest(
	r *http.Request,
	endpoint string,
	form url.Values,
	authMethod string,
) (*http.Request, error) {
	switch authMethod {
	case "client_secret_post":
		form.Set("client_id", p.config.ClientID)
		form.Set("client_secret", p.config.ClientSecret)
	case "private_key_jwt", "client_secret_jwt":
		assertion, err := p.clientAssertion(endpoint, authMethod)
		if err != nil {
			return nil, err
		}
		form.Set("client_id", p.config.ClientID)
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", assertion)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authMethod == "client_secret_basic" {
		req.SetBasicAuth(p.config.ClientID, p.config.ClientSecret)
	}
	return req, nil
}

func (p *Plugin) clientAssertion(audience, authMethod string) (string, error) {
	jti, err := randomURLValue(16)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	header := map[string]any{"typ": "JWT"}
	claims := map[string]any{
		"iss": p.config.ClientID,
		"sub": p.config.ClientID,
		"aud": audience,
		"jti": jti,
		"iat": now,
		"exp": now + int64(p.config.ClientJWTAssertionExpiresIn),
	}
	if authMethod == "private_key_jwt" {
		header["alg"] = "RS256"
		if p.config.ClientRSAPrivateKeyID != "" {
			header["kid"] = p.config.ClientRSAPrivateKeyID
		}
	} else {
		header["alg"] = "HS256"
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	var signature []byte
	if authMethod == "private_key_jwt" {
		if p.clientRSAPrivateKey == nil {
			return "", errors.New("client_rsa_private_key is required for private_key_jwt")
		}
		digest := sha256.Sum256([]byte(unsigned))
		signature, err = rsa.SignPKCS1v15(rand.Reader, p.clientRSAPrivateKey, crypto.SHA256, digest[:])
		if err != nil {
			return "", err
		}
	} else {
		mac := hmac.New(sha256.New, []byte(p.config.ClientSecret))
		_, _ = mac.Write([]byte(unsigned))
		signature = mac.Sum(nil)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func validClientAuthMethod(method string) bool {
	return method == "client_secret_basic" || method == "client_secret_post" ||
		method == "private_key_jwt" || method == "client_secret_jwt"
}

func parseRSAPrivateKey(privateKeyBytes []byte) (*rsa.PrivateKey, error) {
	if block, _ := pem.Decode(privateKeyBytes); block != nil {
		privateKeyBytes = block.Bytes
	}
	if privateKey, err := x509.ParsePKCS1PrivateKey(privateKeyBytes); err == nil {
		return privateKey, nil
	}
	privateKey, err := x509.ParsePKCS8PrivateKey(privateKeyBytes)
	if err != nil {
		return nil, err
	}
	rsaPrivateKey, ok := privateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return rsaPrivateKey, nil
}

func (p *Plugin) userinfo(r *http.Request, accessToken string) (string, error) {
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.UserinfoEndpoint == "" {
		return "", err
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, discovery.UserinfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("userinfo endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return "", errors.New("invalid userinfo response")
	}
	return string(body), nil
}

func (p *Plugin) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := p.readSession(r)
	p.clearSession(w, session)
	if p.config.RevokeTokensOnLogout && session != nil {
		p.revokeTokens(r, *session)
	}
	discovery, err := p.discoveryDoc()
	if err == nil && discovery.EndSessionEndpoint != "" {
		logoutURL, parseErr := url.Parse(discovery.EndSessionEndpoint)
		if parseErr != nil {
			http.Error(w, "invalid end session endpoint", http.StatusBadGateway)
			return
		}
		p.appendPostLogoutRedirectURI(logoutURL)
		http.Redirect(w, r, logoutURL.String(), http.StatusFound)
		return
	}
	if p.config.PostLogoutRedirectURI != "" {
		// Mirrors lua-resty-openidc's logout(): even without an end_session_endpoint,
		// it still appends post_logout_redirect_uri as a query parameter on the
		// fallback redirect target instead of a bare redirect.
		fallbackURL, parseErr := url.Parse(p.config.PostLogoutRedirectURI)
		if parseErr == nil {
			p.appendPostLogoutRedirectURI(fallbackURL)
			http.Redirect(w, r, fallbackURL.String(), http.StatusFound)
			return
		}
		http.Redirect(w, r, p.config.PostLogoutRedirectURI, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (p *Plugin) appendPostLogoutRedirectURI(target *url.URL) {
	if p.config.PostLogoutRedirectURI == "" {
		return
	}
	query := target.Query()
	query.Set("post_logout_redirect_uri", p.config.PostLogoutRedirectURI)
	target.RawQuery = query.Encode()
}

func (p *Plugin) revokeTokens(r *http.Request, session sessionData) {
	discovery, err := p.discoveryDoc()
	if err != nil || discovery.RevocationEndpoint == "" {
		return
	}
	if session.RefreshToken != "" {
		_ = p.revokeToken(r, discovery.RevocationEndpoint, "refresh_token", session.RefreshToken)
	}
	if session.AccessToken != "" {
		_ = p.revokeToken(r, discovery.RevocationEndpoint, "access_token", session.AccessToken)
	}
}

func (p *Plugin) revokeToken(r *http.Request, endpoint, tokenTypeHint, token string) error {
	form := url.Values{}
	form.Set("token", token)
	form.Set("token_type_hint", tokenTypeHint)
	resp, err := p.postTokenForm(r, endpoint, form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("revocation endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) redirectURI(r *http.Request) string {
	if p.config.RedirectURI != "" {
		return p.config.RedirectURI
	}
	const suffix = "/.apisix/redirect"
	path := r.URL.Path
	if !strings.HasSuffix(path, suffix) {
		path = strings.TrimSuffix(path, "/") + suffix
	}
	host := r.Host
	scheme := ""
	if apisixctx.IsTrustedProxy(r) {
		forwardedHost, forwardedProto := forwardedAuthority(r.Header.Get("Forwarded"))
		if forwardedHost != "" {
			host = forwardedHost
		} else if forwardedHost := firstForwardedValue(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
			host = forwardedHost
		}
		if forwardedProto != "" {
			scheme = forwardedProto
		} else {
			scheme = firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
		}
	}
	if scheme == "" {
		scheme = r.URL.Scheme
	}
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + host + path
}

func forwardedAuthority(header string) (string, string) {
	first := firstForwardedValue(header)
	if first == "" {
		return "", ""
	}
	var host string
	var proto string
	for parameter := range strings.SplitSeq(first, ";") {
		key, value, ok := strings.Cut(parameter, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(key), "host"):
			host = value
		case strings.EqualFold(strings.TrimSpace(key), "proto"):
			proto = value
		}
	}
	return host, proto
}

func firstForwardedValue(value string) string {
	first, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(first)
}

func (p *Plugin) isRedirectCallback(r *http.Request, redirectURI string) bool {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return false
	}
	return parsed.Path == r.URL.Path && parsed.Path != ""
}

func (p *Plugin) flowExpiry(now time.Time) time.Time {
	expiresAt := now.Add(10 * time.Minute)
	if p.config.Session.AbsoluteTimeout > 0 {
		absoluteExpiry := now.Add(time.Duration(p.config.Session.AbsoluteTimeout) * time.Second)
		if absoluteExpiry.Before(expiresAt) {
			expiresAt = absoluteExpiry
		}
	}
	return expiresAt
}
