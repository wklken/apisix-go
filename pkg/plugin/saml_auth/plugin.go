package saml_auth

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config      Config
	lifecycleMu sync.RWMutex

	spPrivateKey        secret.Value
	legacySPPrivateKey  *store.ResolvedSecret
	sessionSecret       secret.Value
	sessionSecretSet    bool
	sessionFallbacks    []secret.Value
	legacySessionSecret *store.ResolvedSecret
	legacyFallbacks     []*store.ResolvedSecret
	secretsPrepared     bool
	retired             bool

	// spKeyPair is staged during secret materialization. PostInit consumes it
	// without reading the public private-key descriptor as PEM.
	spKeyPair     *samlKeyPair
	spIDPMetadata *saml.EntityDescriptor
}

// samlKeyPair is the parsed SP certificate and private key.
type samlKeyPair struct {
	cert *x509.Certificate
	key  crypto.Signer
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
      "type": "string"
    },
    "secret_fallbacks": {
      "type": "array",
      "items": {
        "type": "string"
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
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired || !p.secretsPrepared || p.spKeyPair == nil {
		return secret.ErrCredentialUnavailable
	}
	if p.config.AuthProtocolBindingMethod == "" {
		p.config.AuthProtocolBindingMethod = "HTTP-Redirect"
	}
	p.spIDPMetadata = &saml.EntityDescriptor{
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

	return nil
}

func (p *Plugin) MaterializeScopedSecrets(ctx context.Context, access base.ScopedSecretAccess) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}

	privateKey, privateDescriptor, keyPair, err := p.materializeScopedPrivateKey(
		ctx, access, p.config.SPPrivateKey,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	sessionSecret, sessionDescriptor, err := materializeScopedSAMLSessionSecret(
		ctx, access, "secret", p.config.Secret,
	)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	fallbacks := make([]secret.Value, len(p.config.SecretFallbacks))
	fallbackDescriptors := make([]string, len(p.config.SecretFallbacks))
	for i, raw := range p.config.SecretFallbacks {
		fallback, descriptor, fallbackErr := materializeScopedSAMLSessionSecret(
			ctx, access, "secret_fallbacks", raw,
		)
		if fallbackErr != nil {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = fallback
		fallbackDescriptors[i] = descriptor
	}

	p.spPrivateKey = privateKey
	p.sessionSecret = sessionSecret
	p.sessionSecretSet = true
	p.sessionFallbacks = fallbacks
	p.spKeyPair = keyPair
	p.config.SPPrivateKey = privateDescriptor
	p.config.Secret = sessionDescriptor
	p.config.SecretFallbacks = fallbackDescriptors
	p.secretsPrepared = true
	return nil
}

func (p *Plugin) materializeScopedPrivateKey(
	ctx context.Context,
	access base.ScopedSecretAccess,
	raw string,
) (secret.Value, string, *samlKeyPair, error) {
	value, err := access.Materialize(ctx, "sp_private_key", raw)
	if err != nil {
		return secret.Value{}, "", nil, secret.ErrCredentialUnavailable
	}
	var keyPair *samlKeyPair
	if err := value.Use(func(plaintext string) error {
		cert, key, parseErr := parseKeyPair(p.config.SPCert, plaintext)
		if parseErr != nil {
			return secret.ErrCredentialUnavailable
		}
		keyPair = &samlKeyPair{cert: cert, key: key}
		return nil
	}); err != nil {
		return secret.Value{}, "", nil, secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.Value{}, "", nil, secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), keyPair, nil
}

func materializeScopedSAMLSessionSecret(
	ctx context.Context,
	access base.ScopedSecretAccess,
	field string,
	raw string,
) (secret.Value, string, error) {
	value, err := access.Materialize(ctx, field, raw)
	if err != nil || value.Use(validateSAMLSessionSecret) != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.Value{}, "", secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), nil
}

func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}

	privateKey, privateDescriptor, keyPair, err := p.materializeLegacyPrivateKey(p.config.SPPrivateKey)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	sessionSecret, sessionDescriptor, err := p.materializeLegacySecret(
		p.config.Secret, "secret", validateSAMLSessionSecret,
	)
	if err != nil {
		privateKey.Destroy()
		return secret.ErrCredentialUnavailable
	}
	fallbacks := make([]*store.ResolvedSecret, len(p.config.SecretFallbacks))
	fallbackDescriptors := make([]string, len(p.config.SecretFallbacks))
	installed := false
	defer func() {
		if installed {
			return
		}
		privateKey.Destroy()
		sessionSecret.Destroy()
		for _, fallback := range fallbacks {
			fallback.Destroy()
		}
	}()
	for i, raw := range p.config.SecretFallbacks {
		fallback, descriptor, fallbackErr := p.materializeLegacySecret(
			raw, "secret_fallbacks", validateSAMLSessionSecret,
		)
		if fallbackErr != nil {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = fallback
		fallbackDescriptors[i] = descriptor
	}

	p.legacySPPrivateKey = privateKey
	p.legacySessionSecret = sessionSecret
	p.legacyFallbacks = fallbacks
	p.spKeyPair = keyPair
	p.config.SPPrivateKey = privateDescriptor
	p.config.Secret = sessionDescriptor
	p.config.SecretFallbacks = fallbackDescriptors
	p.secretsPrepared = true
	installed = true
	return nil
}

func (p *Plugin) materializeLegacyPrivateKey(raw string) (*store.ResolvedSecret, string, *samlKeyPair, error) {
	value, descriptor, err := p.materializeLegacySecret(raw, "sp_private_key", func(plaintext string) error {
		_, _, parseErr := parseKeyPair(p.config.SPCert, plaintext)
		return parseErr
	})
	if err != nil {
		return nil, "", nil, secret.ErrCredentialUnavailable
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	cert, key, err := parseKeyPair(p.config.SPCert, string(plaintext))
	if err != nil {
		value.Destroy()
		return nil, "", nil, secret.ErrCredentialUnavailable
	}
	return value, descriptor, &samlKeyPair{cert: cert, key: key}, nil
}

func (p *Plugin) materializeLegacySecret(
	raw string,
	field string,
	validate func(string) error,
) (*store.ResolvedSecret, string, error) {
	if resolver := p.DataEncryption(); resolver.Configured() {
		raw = resolver.ResolveOptionalForContext(raw, name+"."+field)
	}
	value, err := store.MaterializeSecret(raw)
	if err != nil {
		return nil, "", secret.ErrCredentialUnavailable
	}
	plaintext := value.Bytes()
	defer clear(plaintext)
	if err := validate(string(plaintext)); err != nil {
		value.Destroy()
		return nil, "", secret.ErrCredentialUnavailable
	}
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		value.Destroy()
		return nil, "", secret.ErrCredentialUnavailable
	}
	return value, descriptor.String(), nil
}

func validateSAMLSessionSecret(plaintext string) error {
	length := utf8.RuneCountInString(plaintext)
	if length < 8 || length > 32 {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

// Stop retires this immutable generation after all active handlers finish.
func (p *Plugin) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return
	}
	p.retired = true
	p.spIDPMetadata = nil
	p.spKeyPair = nil
	if p.legacySPPrivateKey != nil {
		p.legacySPPrivateKey.Destroy()
		p.legacySPPrivateKey = nil
	}
	if p.legacySessionSecret != nil {
		p.legacySessionSecret.Destroy()
		p.legacySessionSecret = nil
	}
	for i, fallback := range p.legacyFallbacks {
		fallback.Destroy()
		p.legacyFallbacks[i] = nil
	}
	p.legacyFallbacks = nil
	p.spPrivateKey = secret.Value{}
	p.sessionSecret = secret.Value{}
	p.sessionSecretSet = false
	for i := range p.sessionFallbacks {
		p.sessionFallbacks[i] = secret.Value{}
	}
	p.sessionFallbacks = nil
	p.secretsPrepared = false
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
		p.lifecycleMu.RLock()
		defer p.lifecycleMu.RUnlock()
		if p.retired || !p.secretsPrepared || p.spKeyPair == nil || p.spIDPMetadata == nil {
			http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == base.CallbackPath(p.config.LogoutCallbackURI) {
			apisixctx.RegisterSensitiveQueryName(r, "SAMLRequest")
			apisixctx.RegisterSensitiveQueryName(r, "SAMLResponse")
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

		if r.URL.Path == base.CallbackPath(p.config.LogoutURI) {
			p.logout(w, r)
			return
		}

		if r.URL.Path == base.CallbackPath(p.config.LoginCallbackURI) {
			apisixctx.RegisterSensitiveQueryName(r, "SAMLResponse")
			if r.FormValue("SAMLResponse") != "" {
				p.handleCallback(w, r)
				return
			}
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
	sp, err := p.serviceProviderLocked(r)
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

	sp, err := p.serviceProviderLocked(r)
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
	sp, err := p.serviceProviderLocked(r)
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
	sp, err := p.serviceProviderLocked(r)
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
	sp, err := p.serviceProviderLocked(r)
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
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.retired || !p.secretsPrepared || p.spKeyPair == nil || p.spIDPMetadata == nil {
		return nil, secret.ErrCredentialUnavailable
	}
	return p.serviceProviderLocked(r)
}

func (p *Plugin) serviceProviderLocked(r *http.Request) (*saml.ServiceProvider, error) {
	acsURL, err := absoluteURL(r, p.config.LoginCallbackURI)
	if err != nil {
		return nil, err
	}
	sloURL, err := absoluteURL(r, p.config.LogoutCallbackURI)
	if err != nil {
		return nil, err
	}

	return &saml.ServiceProvider{
		EntityID:          p.config.SPIssuer,
		Key:               p.spKeyPair.key,
		Certificate:       p.spKeyPair.cert,
		AcsURL:            *acsURL,
		SloURL:            *sloURL,
		IDPMetadata:       p.spIDPMetadata,
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
	defer clear(payload)
	var signed string
	if err := p.useSessionSecretsLocked(func(current string, _ []string) error {
		signed = base.SignRawSessionValue(payload, current)
		return nil
	}); err != nil {
		return nil, secret.ErrCredentialUnavailable
	}
	return &http.Cookie{
		Name:     requestCookieName(p.sessionFingerprint(), stateID),
		Value:    signed,
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
	var state requestState
	err = p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		payload, ok := base.VerifyRawSessionValue(cookie.Value, current, fallbacks)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		defer clear(payload)
		return json.Unmarshal(payload, &state)
	})
	if err != nil {
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
	defer clear(payload)
	var signed string
	if err := p.useSessionSecretsLocked(func(current string, _ []string) error {
		signed = base.SignRawSessionValue(payload, current)
		return nil
	}); err != nil {
		return nil, secret.ErrCredentialUnavailable
	}
	return &http.Cookie{
		Name:     logoutCookieName(p.sessionFingerprint(), stateID),
		Value:    signed,
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
	var state logoutState
	err = p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		payload, ok := base.VerifyRawSessionValue(cookie.Value, current, fallbacks)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		defer clear(payload)
		return json.Unmarshal(payload, &state)
	})
	if err != nil {
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
	var session sessionPayload
	err = p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		payload, ok := base.VerifyRawSessionValue(cookie.Value, current, fallbacks)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		defer clear(payload)
		return json.Unmarshal(payload, &session)
	})
	if err != nil {
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
	defer clear(payload)
	var signed string
	if err := p.useSessionSecretsLocked(func(current string, _ []string) error {
		signed = base.SignRawSessionValue(payload, current)
		return nil
	}); err != nil {
		return nil, secret.ErrCredentialUnavailable
	}
	return &http.Cookie{
		Name:     sessionCookieName(p.sessionFingerprint()),
		Value:    signed,
		Path:     "/",
		HttpOnly: true,
		Secure:   p.forceSecureCookies(),
		SameSite: p.sameSiteMode(),
		MaxAge:   int(sessionLifetime.Seconds()),
	}, nil
}

func (p *Plugin) useSessionSecretsLocked(use func(string, []string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.sessionSecretSet {
		return p.sessionSecret.Use(func(current string) error {
			fallbacks := make([]string, len(p.sessionFallbacks))
			defer clearSAMLStrings(fallbacks)
			return useScopedSAMLFallbacks(p.sessionFallbacks, fallbacks, 0, func() error {
				return use(current, fallbacks)
			})
		})
	}
	if p.legacySessionSecret == nil {
		return secret.ErrCredentialUnavailable
	}
	current := p.legacySessionSecret.Bytes()
	if len(current) == 0 {
		return secret.ErrCredentialUnavailable
	}
	defer clear(current)
	fallbackBytes := make([][]byte, len(p.legacyFallbacks))
	fallbacks := make([]string, len(p.legacyFallbacks))
	defer func() {
		for i := range fallbackBytes {
			clear(fallbackBytes[i])
		}
		clearSAMLStrings(fallbacks)
	}()
	for i, owner := range p.legacyFallbacks {
		if owner == nil {
			return secret.ErrCredentialUnavailable
		}
		fallbackBytes[i] = owner.Bytes()
		if len(fallbackBytes[i]) == 0 {
			return secret.ErrCredentialUnavailable
		}
		fallbacks[i] = string(fallbackBytes[i])
	}
	return use(string(current), fallbacks)
}

func useScopedSAMLFallbacks(
	values []secret.Value,
	plaintext []string,
	index int,
	use func() error,
) error {
	if index == len(values) {
		return use()
	}
	return values[index].Use(func(value string) error {
		plaintext[index] = value
		defer func() { plaintext[index] = "" }()
		return useScopedSAMLFallbacks(values, plaintext, index+1, use)
	})
}

func clearSAMLStrings(values []string) {
	for i := range values {
		values[i] = ""
	}
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
	apisixctx.RegisterSensitiveQueryName(r, field)
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
