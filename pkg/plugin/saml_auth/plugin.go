package saml_auth

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 2598
	name     = "saml-auth"

	requestCookiePrefix = "SAML_REQUEST_"
	sessionCookiePrefix = "SAML_SESSION_"
	stateLifetime       = 10 * time.Minute
	sessionLifetime     = 24 * time.Hour
	rsaSHA256Method     = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
)

const schema = `
{
  "type": "object",
  "properties": {
    "sp_issuer": {
      "type": "string"
    },
    "idp_uri": {
      "type": "string"
    },
    "idp_cert": {
      "type": "string"
    },
    "login_callback_uri": {
      "type": "string"
    },
    "logout_uri": {
      "type": "string"
    },
    "logout_callback_uri": {
      "type": "string"
    },
    "logout_redirect_uri": {
      "type": "string"
    },
    "sp_cert": {
      "type": "string"
    },
    "sp_private_key": {
      "type": "string"
    },
    "auth_protocol_binding_method": {
      "type": "string",
      "default": "HTTP-Redirect",
      "enum": ["HTTP-Redirect", "HTTP-POST"]
    },
    "secret": {
      "type": "string",
      "minLength": 8,
      "maxLength": 32
    },
    "secret_fallbacks": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 8,
        "maxLength": 32
      }
    }
  },
  "required": [
    "sp_issuer",
    "idp_uri",
    "idp_cert",
    "login_callback_uri",
    "logout_uri",
    "logout_callback_uri",
    "logout_redirect_uri",
    "sp_cert",
    "sp_private_key",
    "secret"
  ]
}
`

type Config struct {
	SPIssuer                  string   `json:"sp_issuer"`
	IDPURI                    string   `json:"idp_uri"`
	IDPCert                   string   `json:"idp_cert"`
	LoginCallbackURI          string   `json:"login_callback_uri"`
	LogoutURI                 string   `json:"logout_uri"`
	LogoutCallbackURI         string   `json:"logout_callback_uri"`
	LogoutRedirectURI         string   `json:"logout_redirect_uri"`
	SPCert                    string   `json:"sp_cert"`
	SPPrivateKey              string   `json:"sp_private_key"`
	AuthProtocolBindingMethod string   `json:"auth_protocol_binding_method,omitempty"`
	Secret                    string   `json:"secret"`
	SecretFallbacks           []string `json:"secret_fallbacks,omitempty"`
}

type requestState struct {
	RequestID   string `json:"request_id"`
	OriginalURI string `json:"original_uri"`
	ExpiresAt   int64  `json:"expires_at"`
}

type externalUser struct {
	NameID     string              `json:"name_id,omitempty"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type sessionPayload struct {
	User      externalUser `json:"user"`
	ExpiresAt int64        `json:"expires_at"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.AuthProtocolBindingMethod == "" {
		p.config.AuthProtocolBindingMethod = "HTTP-Redirect"
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == callbackPath(p.config.LogoutCallbackURI) {
			p.deleteCookie(w, sessionCookieName(p.sessionFingerprint()))
			http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
			return
		}

		if r.URL.Path == callbackPath(p.config.LogoutURI) {
			p.logout(w, r)
			return
		}

		if r.URL.Path == callbackPath(p.config.LoginCallbackURI) && r.FormValue("SAMLResponse") != "" {
			p.handleCallback(w, r)
			return
		}

		if user, ok := p.sessionUser(r); ok {
			p.attachUser(r, user)
			next.ServeHTTP(w, r)
			return
		}

		p.startAuthentication(w, r)
	})
}

func (p *Plugin) startAuthentication(w http.ResponseWriter, r *http.Request) {
	sp, err := p.serviceProvider(r)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("create saml object failed"), http.StatusInternalServerError)
		return
	}

	binding := saml.HTTPRedirectBinding
	if p.config.AuthProtocolBindingMethod == "HTTP-POST" {
		binding = saml.HTTPPostBinding
	}
	authReq, err := sp.MakeAuthenticationRequest(p.config.IDPURI, binding, saml.HTTPPostBinding)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
		return
	}

	stateID := randomState()
	state := requestState{
		RequestID:   authReq.ID,
		OriginalURI: r.URL.RequestURI(),
		ExpiresAt:   time.Now().Add(stateLifetime).Unix(),
	}
	cookie, err := p.requestCookie(stateID, state)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)

	if binding == saml.HTTPPostBinding {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(authReq.Post(stateID))
		return
	}

	redirectURL, err := authReq.Redirect(stateID, sp)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (p *Plugin) handleCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, util.BuildMessageResponse("invalid saml response"), http.StatusUnauthorized)
		return
	}

	stateID := r.Form.Get("RelayState")
	state, ok := p.requestState(r, stateID)
	if !ok {
		http.Error(w, util.BuildMessageResponse("invalid callback state"), http.StatusUnauthorized)
		return
	}

	sp, err := p.serviceProvider(r)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("create saml object failed"), http.StatusInternalServerError)
		return
	}
	assertion, err := sp.ParseResponse(r, []string{state.RequestID})
	if err != nil {
		http.Error(w, util.BuildMessageResponse("invalid saml response"), http.StatusUnauthorized)
		return
	}

	cookie, err := p.sessionCookie(userFromAssertion(assertion))
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, cookie)
	p.deleteCookie(w, requestCookieName(p.sessionFingerprint(), stateID))

	location := state.OriginalURI
	if !safeRedirect(location) {
		location = "/"
	}
	http.Redirect(w, r, location, http.StatusFound)
}

func (p *Plugin) logout(w http.ResponseWriter, r *http.Request) {
	user, ok := p.sessionUser(r)
	if !ok {
		http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
		return
	}

	p.deleteCookie(w, sessionCookieName(p.sessionFingerprint()))
	sp, err := p.serviceProvider(r)
	if err != nil || user.NameID == "" {
		http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
		return
	}
	redirectURL, err := sp.MakeRedirectLogoutRequest(user.NameID, p.config.LogoutRedirectURI)
	if err != nil {
		http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
		return
	}
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (p *Plugin) serviceProvider(r *http.Request) (*saml.ServiceProvider, error) {
	cert, key, err := parseKeyPair(p.config.SPCert, p.config.SPPrivateKey)
	if err != nil {
		return nil, err
	}
	acsURL, err := absoluteURL(r, p.config.LoginCallbackURI)
	if err != nil {
		return nil, err
	}
	sloURL, err := absoluteURL(r, p.config.LogoutCallbackURI)
	if err != nil {
		return nil, err
	}

	idpMetadata := &saml.EntityDescriptor{
		EntityID: p.config.IDPURI,
		IDPSSODescriptors: []saml.IDPSSODescriptor{
			{
				SSODescriptor: saml.SSODescriptor{
					RoleDescriptor: saml.RoleDescriptor{
						ProtocolSupportEnumeration: "urn:oasis:names:tc:SAML:2.0:protocol",
						KeyDescriptors: []saml.KeyDescriptor{
							{
								Use: "signing",
								KeyInfo: saml.KeyInfo{
									X509Data: saml.X509Data{
										X509Certificates: []saml.X509Certificate{
											{Data: certificateData(p.config.IDPCert)},
										},
									},
								},
							},
						},
					},
					SingleLogoutServices: []saml.Endpoint{
						{Binding: saml.HTTPRedirectBinding, Location: p.config.IDPURI},
						{Binding: saml.HTTPPostBinding, Location: p.config.IDPURI},
					},
				},
				SingleSignOnServices: []saml.Endpoint{
					{Binding: saml.HTTPRedirectBinding, Location: p.config.IDPURI},
					{Binding: saml.HTTPPostBinding, Location: p.config.IDPURI},
				},
			},
		},
	}

	return &saml.ServiceProvider{
		EntityID:          p.config.SPIssuer,
		Key:               key,
		Certificate:       cert,
		AcsURL:            *acsURL,
		SloURL:            *sloURL,
		IDPMetadata:       idpMetadata,
		SignatureMethod:   rsaSHA256Method,
		AuthnNameIDFormat: saml.UnspecifiedNameIDFormat,
		LogoutBindings:    []string{saml.HTTPRedirectBinding, saml.HTTPPostBinding},
	}, nil
}

func parseKeyPair(certPEM string, keyPEM string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, nil, err
	}
	if len(pair.Certificate) == 0 {
		return nil, nil, fmt.Errorf("missing certificate")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("private key does not implement crypto.Signer")
	}
	return cert, key, nil
}

func certificateData(cert string) string {
	block, _ := pem.Decode([]byte(cert))
	if block != nil {
		return base64.StdEncoding.EncodeToString(block.Bytes)
	}
	return strings.Join(strings.Fields(cert), "")
}

func (p *Plugin) requestCookie(stateID string, state requestState) (*http.Cookie, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     requestCookieName(p.sessionFingerprint(), stateID),
		Value:    p.signValue(payload),
		Path:     "/",
		HttpOnly: true,
		Secure:   p.forceSecureCookies(),
		SameSite: p.sameSiteMode(),
		MaxAge:   int(stateLifetime.Seconds()),
	}, nil
}

func (p *Plugin) requestState(r *http.Request, stateID string) (requestState, bool) {
	if stateID == "" {
		return requestState{}, false
	}
	cookie, err := r.Cookie(requestCookieName(p.sessionFingerprint(), stateID))
	if err != nil || cookie.Value == "" {
		return requestState{}, false
	}
	payload, ok := p.verifySignedValue(cookie.Value)
	if !ok {
		return requestState{}, false
	}
	var state requestState
	if err := json.Unmarshal(payload, &state); err != nil {
		return requestState{}, false
	}
	if state.ExpiresAt <= time.Now().Unix() || state.RequestID == "" {
		return requestState{}, false
	}
	return state, true
}

func (p *Plugin) sessionUser(r *http.Request) (externalUser, bool) {
	cookie, err := r.Cookie(sessionCookieName(p.sessionFingerprint()))
	if err != nil || cookie.Value == "" {
		return externalUser{}, false
	}
	payload, ok := p.verifySignedValue(cookie.Value)
	if !ok {
		return externalUser{}, false
	}
	var session sessionPayload
	if err := json.Unmarshal(payload, &session); err != nil {
		return externalUser{}, false
	}
	if session.ExpiresAt <= time.Now().Unix() || session.User.NameID == "" {
		return externalUser{}, false
	}
	return session.User, true
}

func (p *Plugin) sessionCookie(user externalUser) (*http.Cookie, error) {
	payload, err := json.Marshal(sessionPayload{
		User:      user,
		ExpiresAt: time.Now().Add(sessionLifetime).Unix(),
	})
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     sessionCookieName(p.sessionFingerprint()),
		Value:    p.signValue(payload),
		Path:     "/",
		HttpOnly: true,
		Secure:   p.forceSecureCookies(),
		SameSite: p.sameSiteMode(),
		MaxAge:   int(sessionLifetime.Seconds()),
	}, nil
}

func (p *Plugin) deleteCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "deleted",
		Path:     "/",
		HttpOnly: true,
		Secure:   p.forceSecureCookies(),
		SameSite: p.sameSiteMode(),
		MaxAge:   -1,
	})
}

func (p *Plugin) attachUser(r *http.Request, user externalUser) {
	raw, err := json.Marshal(user)
	if err == nil {
		r.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
	}
	if vars := apisixctx.GetApisixVars(r); vars != nil {
		vars["$external_user"] = user
	}
}

func (p *Plugin) signValue(payload []byte) string {
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(p.config.Secret))
	mac.Write(payload)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + signature
}

func (p *Plugin) verifySignedValue(signed string) ([]byte, bool) {
	dot := strings.Index(signed, ".")
	if dot <= 0 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(signed[:dot])
	if err != nil {
		return nil, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signed[dot+1:])
	if err != nil {
		return nil, false
	}

	for _, secret := range append([]string{p.config.Secret}, p.config.SecretFallbacks...) {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) == 1 {
			return payload, true
		}
	}
	return nil, false
}

func (p *Plugin) sessionFingerprint() string {
	sum := sha256.Sum256([]byte(p.config.SPIssuer + "|" + p.config.IDPURI + "|" + p.config.LoginCallbackURI))
	return hex.EncodeToString(sum[:])[:16]
}

func (p *Plugin) forceSecureCookies() bool {
	return p.config.AuthProtocolBindingMethod == "HTTP-POST"
}

func (p *Plugin) sameSiteMode() http.SameSite {
	if p.forceSecureCookies() {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

func userFromAssertion(assertion *saml.Assertion) externalUser {
	user := externalUser{Attributes: map[string][]string{}}
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		user.NameID = assertion.Subject.NameID.Value
	}
	for _, statement := range assertion.AttributeStatements {
		for _, attr := range statement.Attributes {
			key := attr.FriendlyName
			if key == "" {
				key = attr.Name
			}
			if key == "" {
				continue
			}
			for _, value := range attr.Values {
				switch {
				case value.Value != "":
					user.Attributes[key] = append(user.Attributes[key], value.Value)
				case value.NameID != nil && value.NameID.Value != "":
					user.Attributes[key] = append(user.Attributes[key], value.NameID.Value)
				}
			}
		}
	}
	if len(user.Attributes) == 0 {
		user.Attributes = nil
	}
	return user
}

func absoluteURL(r *http.Request, rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.IsAbs() {
		return parsed, nil
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !strings.HasPrefix(rawURL, "/") {
		rawURL = "/" + rawURL
	}
	return url.Parse(scheme + "://" + r.Host + rawURL)
}

func callbackPath(callbackURI string) string {
	parsed, err := url.Parse(callbackURI)
	if err != nil || !parsed.IsAbs() {
		return callbackURI
	}
	if parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

func safeRedirect(uri string) bool {
	if uri == "" || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return false
	}
	return !strings.ContainsAny(uri, "\\\r\n")
}

func requestCookieName(fingerprint string, stateID string) string {
	return requestCookiePrefix + fingerprint + "_" + stateID
}

func sessionCookieName(fingerprint string) string {
	return sessionCookiePrefix + fingerprint
}

func randomState() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(raw)
}
