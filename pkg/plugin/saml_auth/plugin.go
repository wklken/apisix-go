package saml_auth

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
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
	logoutCookiePrefix  = "SAML_LOGOUT_"
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
    "idp_entity_id": {
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
	IDPEntityID               string   `json:"idp_entity_id,omitempty"`
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

type logoutState struct {
	RequestID string `json:"request_id"`
	ExpiresAt int64  `json:"expires_at"`
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

func (c Config) idpEntityID() string {
	if c.IDPEntityID != "" {
		return c.IDPEntityID
	}
	return c.IDPURI
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == callbackPath(p.config.LogoutCallbackURI) {
			switch {
			case r.FormValue("SAMLRequest") != "":
				p.handleLogoutRequest(w, r)
			case r.FormValue("SAMLResponse") != "":
				p.handleLogoutResponse(w, r)
			default:
				http.Error(w, util.BuildMessageResponse("invalid saml logout message"), http.StatusUnauthorized)
			}
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

	stateID, err := randomState(rand.Reader)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
		return
	}
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
		logger.Errorf("saml authenticate failed: %v", samlAuthenticationDiagnostic(err))
		http.Error(w, util.BuildMessageResponse("saml authentication failed"), http.StatusInternalServerError)
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

func samlAuthenticationDiagnostic(err error) error {
	var invalidResponse *saml.InvalidResponseError
	if errors.As(err, &invalidResponse) && invalidResponse.PrivateErr != nil {
		return invalidResponse.PrivateErr
	}
	return err
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
	stateID, err := randomState(rand.Reader)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml logout failed"), http.StatusInternalServerError)
		return
	}
	logoutRequest, redirectURL, err := signedRedirectLogoutRequest(
		sp,
		p.config.IDPURI,
		user.NameID,
		stateID,
	)
	if err != nil {
		http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
		return
	}
	stateCookie, err := p.logoutCookie(stateID, logoutState{
		RequestID: logoutRequest.ID,
		ExpiresAt: time.Now().Add(stateLifetime).Unix(),
	})
	if err != nil {
		http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
		return
	}
	http.SetCookie(w, stateCookie)
	http.Redirect(w, r, redirectURL.String(), http.StatusFound)
}

func (p *Plugin) handleLogoutRequest(w http.ResponseWriter, r *http.Request) {
	sp, err := p.serviceProvider(r)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("create saml object failed"), http.StatusInternalServerError)
		return
	}
	logoutRequest, err := p.validateLogoutRequest(r, sp)
	if err != nil {
		logger.Errorf("saml logout request failed: %v", samlAuthenticationDiagnostic(err))
		http.Error(w, util.BuildMessageResponse("invalid saml logout request"), http.StatusUnauthorized)
		return
	}

	p.deleteCookie(w, sessionCookieName(p.sessionFingerprint()))
	relayState := r.FormValue("RelayState")
	if r.URL.Query().Get("SAMLRequest") != "" {
		_, redirectURL, err := signedRedirectLogoutResponse(
			sp,
			p.config.IDPURI,
			logoutRequest.ID,
			relayState,
		)
		if err != nil {
			http.Error(w, util.BuildMessageResponse("saml logout failed"), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, redirectURL.String(), http.StatusFound)
		return
	}
	form, err := sp.MakePostLogoutResponse(logoutRequest.ID, relayState)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("saml logout failed"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(form)
}

func (p *Plugin) handleLogoutResponse(w http.ResponseWriter, r *http.Request) {
	stateID := r.FormValue("RelayState")
	state, ok := p.logoutState(r, stateID)
	if !ok {
		http.Error(w, util.BuildMessageResponse("invalid saml logout state"), http.StatusUnauthorized)
		return
	}
	sp, err := p.serviceProvider(r)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("create saml object failed"), http.StatusInternalServerError)
		return
	}
	response, err := p.validateLogoutResponse(r, sp)
	if err != nil {
		logger.Errorf("saml logout response failed: %v", samlAuthenticationDiagnostic(err))
		http.Error(w, util.BuildMessageResponse("invalid saml logout response"), http.StatusUnauthorized)
		return
	}
	if response.InResponseTo != state.RequestID {
		http.Error(w, util.BuildMessageResponse("invalid saml logout correlation"), http.StatusUnauthorized)
		return
	}

	p.deleteCookie(w, sessionCookieName(p.sessionFingerprint()))
	p.deleteCookie(w, logoutCookieName(p.sessionFingerprint(), stateID))
	http.Redirect(w, r, p.config.LogoutRedirectURI, http.StatusFound)
}

func (p *Plugin) validateLogoutRequest(r *http.Request, sp *saml.ServiceProvider) (saml.LogoutRequest, error) {
	rawXML, err := decodeSAMLMessage(r, "SAMLRequest")
	if err != nil {
		return saml.LogoutRequest{}, err
	}
	validatedXML := rawXML
	if r.URL.Query().Get("SAMLRequest") != "" {
		if err := verifySAMLRedirectSignature(r.URL.RawQuery, "SAMLRequest", p.config.IDPCert); err != nil {
			return saml.LogoutRequest{}, err
		}
	} else {
		validatedXML, err = validateSignedSAMLXML(rawXML, p.config.IDPCert)
		if err != nil {
			return saml.LogoutRequest{}, err
		}
	}
	var request saml.LogoutRequest
	if err := xml.Unmarshal(validatedXML, &request); err != nil {
		return saml.LogoutRequest{}, fmt.Errorf("decode logout request: %w", err)
	}
	switch {
	case request.ID == "" || request.Version != "2.0":
		return saml.LogoutRequest{}, errors.New("logout request has invalid ID or version")
	case request.Destination != sp.SloURL.String():
		return saml.LogoutRequest{}, fmt.Errorf("logout request Destination does not match %q", sp.SloURL.String())
	case request.Issuer == nil || request.Issuer.Value != p.config.idpEntityID():
		return saml.LogoutRequest{}, fmt.Errorf("logout request issuer does not match %q", p.config.idpEntityID())
	case request.IssueInstant.Add(saml.MaxIssueDelay).Before(time.Now()):
		return saml.LogoutRequest{}, errors.New("logout request has expired")
	case request.IssueInstant.After(time.Now().Add(saml.MaxClockSkew)):
		return saml.LogoutRequest{}, errors.New("logout request IssueInstant is in the future")
	case request.NotOnOrAfter != nil && request.NotOnOrAfter.Add(saml.MaxClockSkew).Before(time.Now()):
		return saml.LogoutRequest{}, errors.New("logout request NotOnOrAfter has expired")
	}
	return request, nil
}

func (p *Plugin) validateLogoutResponse(
	r *http.Request,
	sp *saml.ServiceProvider,
) (saml.LogoutResponse, error) {
	rawXML, err := decodeSAMLMessage(r, "SAMLResponse")
	if err != nil {
		return saml.LogoutResponse{}, err
	}
	validatedXML := rawXML
	if r.URL.Query().Get("SAMLResponse") != "" {
		if err := verifySAMLRedirectSignature(r.URL.RawQuery, "SAMLResponse", p.config.IDPCert); err != nil {
			return saml.LogoutResponse{}, err
		}
	} else {
		validatedXML, err = validateSignedSAMLXML(rawXML, p.config.IDPCert)
		if err != nil {
			return saml.LogoutResponse{}, err
		}
	}
	var response saml.LogoutResponse
	if err := xml.Unmarshal(validatedXML, &response); err != nil {
		return saml.LogoutResponse{}, fmt.Errorf("decode logout response: %w", err)
	}
	switch {
	case response.ID == "" || response.Version != "2.0":
		return saml.LogoutResponse{}, errors.New("logout response has invalid ID or version")
	case response.Destination != sp.SloURL.String():
		return saml.LogoutResponse{}, fmt.Errorf("logout response Destination does not match %q", sp.SloURL.String())
	case response.Issuer == nil || response.Issuer.Value != p.config.idpEntityID():
		return saml.LogoutResponse{}, fmt.Errorf("logout response issuer does not match %q", p.config.idpEntityID())
	case response.IssueInstant.Add(saml.MaxIssueDelay).Before(time.Now()):
		return saml.LogoutResponse{}, errors.New("logout response has expired")
	case response.IssueInstant.After(time.Now().Add(saml.MaxClockSkew)):
		return saml.LogoutResponse{}, errors.New("logout response IssueInstant is in the future")
	case response.Status.StatusCode.Value != saml.StatusSuccess:
		return saml.LogoutResponse{}, errors.New("logout response status is not success")
	}
	return response, nil
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
		EntityID: p.config.idpEntityID(),
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

func (p *Plugin) logoutCookie(stateID string, state logoutState) (*http.Cookie, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{
		Name:     logoutCookieName(p.sessionFingerprint(), stateID),
		Value:    p.signValue(payload),
		Path:     "/",
		HttpOnly: true,
		Secure:   p.forceSecureCookies(),
		SameSite: p.sameSiteMode(),
		MaxAge:   int(stateLifetime.Seconds()),
	}, nil
}

func (p *Plugin) logoutState(r *http.Request, stateID string) (logoutState, bool) {
	if stateID == "" {
		return logoutState{}, false
	}
	cookie, err := r.Cookie(logoutCookieName(p.sessionFingerprint(), stateID))
	if err != nil || cookie.Value == "" {
		return logoutState{}, false
	}
	payload, ok := p.verifySignedValue(cookie.Value)
	if !ok {
		return logoutState{}, false
	}
	var state logoutState
	if err := json.Unmarshal(payload, &state); err != nil {
		return logoutState{}, false
	}
	if state.ExpiresAt <= time.Now().Unix() || state.RequestID == "" {
		return logoutState{}, false
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
	identity := p.config.SPIssuer + "|" + p.config.IDPURI + "|" + p.config.LoginCallbackURI
	if p.config.IDPEntityID != "" {
		identity += "|" + p.config.idpEntityID()
	}
	sum := sha256.Sum256([]byte(identity))
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

func logoutCookieName(fingerprint string, stateID string) string {
	return logoutCookiePrefix + fingerprint + "_" + stateID
}

func sessionCookieName(fingerprint string) string {
	return sessionCookiePrefix + fingerprint
}

func decodeLogoutResponse(rawURL string) (saml.LogoutResponse, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return saml.LogoutResponse{}, fmt.Errorf("parse logout response URL: %w", err)
	}
	rawXML, err := decodeSAMLRedirectValue(parsed.Query().Get("SAMLResponse"))
	if err != nil {
		return saml.LogoutResponse{}, err
	}
	var response saml.LogoutResponse
	if err := xml.Unmarshal(rawXML, &response); err != nil {
		return saml.LogoutResponse{}, fmt.Errorf("decode logout response: %w", err)
	}
	return response, nil
}

func signedRedirectLogoutRequest(
	sp *saml.ServiceProvider,
	destination string,
	nameID string,
	relayState string,
) (*saml.LogoutRequest, *url.URL, error) {
	unsignedSP := *sp
	unsignedSP.SignatureMethod = ""
	request, err := unsignedSP.MakeLogoutRequest(destination, nameID)
	if err != nil {
		return nil, nil, err
	}
	rawXML, err := request.Bytes()
	if err != nil {
		return nil, nil, err
	}
	redirect, err := signedSAMLRedirectURL(destination, "SAMLRequest", rawXML, relayState, sp.Key)
	if err != nil {
		return nil, nil, err
	}
	return request, redirect, nil
}

func signedRedirectLogoutResponse(
	sp *saml.ServiceProvider,
	destination string,
	requestID string,
	relayState string,
) (*saml.LogoutResponse, *url.URL, error) {
	unsignedSP := *sp
	unsignedSP.SignatureMethod = ""
	response, err := unsignedSP.MakeLogoutResponse(destination, requestID)
	if err != nil {
		return nil, nil, err
	}
	rawXML, err := samlElementBytes(response.Element())
	if err != nil {
		return nil, nil, err
	}
	redirect, err := signedSAMLRedirectURL(destination, "SAMLResponse", rawXML, relayState, sp.Key)
	if err != nil {
		return nil, nil, err
	}
	return response, redirect, nil
}

func signedSAMLRedirectURL(
	destination string,
	field string,
	rawXML []byte,
	relayState string,
	signer crypto.Signer,
) (*url.URL, error) {
	if signer == nil {
		return nil, errors.New("SAML Redirect signer is required")
	}
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, 9)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(rawXML); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	signedQuery := field + "=" + url.QueryEscape(base64.StdEncoding.EncodeToString(compressed.Bytes()))
	if relayState != "" {
		signedQuery += "&RelayState=" + url.QueryEscape(relayState)
	}
	signedQuery += "&SigAlg=" + url.QueryEscape(rsaSHA256Method)
	digest := sha256.Sum256([]byte(signedQuery))
	signature, err := signer.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	redirect, err := url.Parse(destination)
	if err != nil {
		return nil, err
	}
	if redirect.RawQuery != "" {
		redirect.RawQuery += "&"
	}
	redirect.RawQuery += signedQuery + "&Signature=" +
		url.QueryEscape(base64.StdEncoding.EncodeToString(signature))
	return redirect, nil
}

func verifySAMLRedirectSignature(rawQuery string, field string, certificatePEM string) error {
	parameters, err := rawRedirectParameters(rawQuery)
	if err != nil {
		return err
	}
	message, ok := parameters[field]
	if !ok {
		return fmt.Errorf("%s is required", field)
	}
	sigAlg, ok := parameters["SigAlg"]
	if !ok {
		return errors.New("SigAlg is required")
	}
	signatureValue, ok := parameters["Signature"]
	if !ok {
		return errors.New("signature is required")
	}
	decodedSigAlg, err := url.QueryUnescape(sigAlg)
	if err != nil || decodedSigAlg != rsaSHA256Method {
		return errors.New("SAML Redirect SigAlg must be rsa-sha256")
	}

	signedQuery := field + "=" + message
	if relayState, present := parameters["RelayState"]; present {
		signedQuery += "&RelayState=" + relayState
	}
	signedQuery += "&SigAlg=" + sigAlg
	decodedSignature, err := url.QueryUnescape(signatureValue)
	if err != nil {
		return fmt.Errorf("decode Redirect signature escaping: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(decodedSignature)
	if err != nil {
		return fmt.Errorf("decode Redirect signature: %w", err)
	}
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		return errors.New("SAML signing certificate is not PEM encoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse SAML signing certificate: %w", err)
	}
	publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("SAML signing certificate does not contain an RSA public key")
	}
	digest := sha256.Sum256([]byte(signedQuery))
	if err := rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature); err != nil {
		return fmt.Errorf("verify Redirect signature: %w", err)
	}
	return nil
}

func rawRedirectParameters(rawQuery string) (map[string]string, error) {
	parameters := make(map[string]string)
	for field := range strings.SplitSeq(rawQuery, "&") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		decodedName, err := url.QueryUnescape(name)
		if err != nil {
			return nil, fmt.Errorf("decode Redirect parameter name: %w", err)
		}
		switch decodedName {
		case "SAMLRequest", "SAMLResponse", "RelayState", "SigAlg", "Signature":
			if _, duplicate := parameters[decodedName]; duplicate {
				return nil, fmt.Errorf("redirect parameter %s is duplicated", decodedName)
			}
			parameters[decodedName] = value
		}
	}
	return parameters, nil
}

func samlElementBytes(element *etree.Element) ([]byte, error) {
	document := etree.NewDocument()
	document.SetRoot(element)
	return document.WriteToBytes()
}

func decodeSAMLMessage(r *http.Request, field string) ([]byte, error) {
	if value := r.URL.Query().Get(field); value != "" {
		return decodeSAMLRedirectValue(value)
	}
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("parse SAML form: %w", err)
	}
	value := r.PostForm.Get(field)
	if value == "" {
		return nil, fmt.Errorf("%s is required", field)
	}
	rawXML, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", field, err)
	}
	if len(rawXML) > 1<<20 {
		return nil, fmt.Errorf("%s exceeds the 1 MiB limit", field)
	}
	return rawXML, nil
}

func decodeSAMLRedirectValue(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("SAML Redirect value is required")
	}
	compressed, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode SAML Redirect value: %w", err)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() {
		_ = reader.Close()
	}()
	rawXML, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("inflate SAML Redirect value: %w", err)
	}
	if len(rawXML) > 1<<20 {
		return nil, errors.New("SAML Redirect value exceeds the 1 MiB limit")
	}
	return rawXML, nil
}

func validateSignedSAMLXML(rawXML []byte, certificatePEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		return nil, errors.New("SAML signing certificate is not PEM encoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse SAML signing certificate: %w", err)
	}
	document := etree.NewDocument()
	if err := document.ReadFromBytes(rawXML); err != nil {
		return nil, fmt.Errorf("parse signed SAML XML: %w", err)
	}
	if document.Root() == nil {
		return nil, errors.New("signed SAML XML has no root element")
	}
	context := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{
		Roots: []*x509.Certificate{certificate},
	})
	validated, err := context.Validate(document.Root())
	if err != nil {
		return nil, fmt.Errorf("verify SAML XML signature: %w", err)
	}
	validatedDocument := etree.NewDocument()
	validatedDocument.SetRoot(validated)
	validatedXML, err := validatedDocument.WriteToBytes()
	if err != nil {
		return nil, fmt.Errorf("serialize validated SAML XML: %w", err)
	}
	return validatedXML, nil
}

func randomState(reader io.Reader) (string, error) {
	raw := make([]byte, 16)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", fmt.Errorf("generate authorization state: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
