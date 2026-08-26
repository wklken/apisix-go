package cas_auth

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/golang-lru/v2/expirable"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/cache/memory"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client            *http.Client
	opts              sessionOptions
	logoutTrustedNets []*net.IPNet

	lifecycleMu     sync.RWMutex
	cookieSecret    secret.Value
	cookieSecretSet bool
	secretsPrepared bool
	retired         bool
}

const (
	priority = 2597
	name     = "cas-auth"

	requestURICookie   = "CAS_REQUEST_URI"
	sessionPrefix      = "CAS_SESSION_"
	sessionLifetime    = time.Hour
	sessionCapacity    = 10_000
	maxLogoutBodyBytes = 64 * 1024
	minCookieSecretLen = 32
)

const schema = `
{
  "type": "object",
  "properties": {
    "idp_uri": {
      "type": "string"
    },
    "cas_callback_uri": {
      "type": "string"
    },
    "logout_uri": {
      "type": "string"
    },
    "logout_trusted_addresses": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "cookie": {
      "type": "object",
      "properties": {
        "secret": {
          "type": "string"
        },
        "secure": {
          "type": "boolean",
          "default": true
        },
        "samesite": {
          "type": "string",
          "enum": ["Lax", "None"],
          "default": "Lax"
        }
      },
      "required": ["secret"]
    }
  },
  "required": ["idp_uri", "cas_callback_uri", "logout_uri", "cookie"]
}
`

type Config struct {
	IDPURI                 string       `json:"idp_uri"`
	CASCallbackURI         string       `json:"cas_callback_uri"`
	LogoutURI              string       `json:"logout_uri"`
	LogoutTrustedAddresses []string     `json:"logout_trusted_addresses,omitempty"`
	Cookie                 CookieConfig `json:"cookie"`
}

type CookieConfig struct {
	Secret   string `json:"secret"`
	Secure   *bool  `json:"secure,omitempty"`
	SameSite string `json:"samesite,omitempty"`
}

type sessionEntry struct {
	fingerprint string
	user        string
}

type sessionOptions struct {
	cookieName  string
	fingerprint string
}

type sessionStore struct {
	mu    sync.Mutex
	cache *expirable.LRU[string, sessionEntry]
}

var processSessions = mustNewSessionStore(sessionCapacity, sessionLifetime)

func newSessionStore(capacity int, ttl time.Duration) (*sessionStore, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("session store capacity must be positive")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("session store TTL must be positive")
	}
	cache, err := memory.NewLRU[string, sessionEntry](capacity, ttl)
	if err != nil {
		return nil, fmt.Errorf("create session store: %w", err)
	}
	return &sessionStore{cache: cache}, nil
}

func mustNewSessionStore(capacity int, ttl time.Duration) *sessionStore {
	store, err := newSessionStore(capacity, ttl)
	if err != nil {
		panic(err)
	}
	return store
}

func (s *sessionStore) put(key string, entry sessionEntry) {
	s.mu.Lock()
	s.cache.Add(key, entry)
	s.mu.Unlock()
}

func (s *sessionStore) refresh(key string, fingerprint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache.Get(key)
	if !ok || entry.fingerprint != fingerprint {
		s.cache.Remove(key)
		return false
	}
	s.cache.Add(key, entry)
	return true
}

func (s *sessionStore) remove(key string) {
	s.mu.Lock()
	s.cache.Remove(key)
	s.mu.Unlock()
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
	if p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	p.logoutTrustedNets = nil
	for _, address := range p.config.LogoutTrustedAddresses {
		_, network, err := net.ParseCIDR(address)
		if err != nil {
			return fmt.Errorf("invalid logout_trusted_addresses entry %q: %w", address, err)
		}
		p.logoutTrustedNets = append(p.logoutTrustedNets, network)
	}
	if len(p.logoutTrustedNets) == 0 {
		logger.Warn(
			"cas-auth back-channel logout is disabled: configure logout_trusted_addresses with trusted IdP CIDRs",
		)
	}
	if p.config.Cookie.Secure == nil {
		secure := true
		p.config.Cookie.Secure = &secure
	}
	if p.config.Cookie.SameSite == "" {
		p.config.Cookie.SameSite = "Lax"
	}
	if p.config.Cookie.SameSite == "None" && !*p.config.Cookie.Secure {
		return fmt.Errorf(`cookie.secure must be true when cookie.samesite is "None"`)
	}
	if p.client == nil {
		p.client = &http.Client{Transport: httpclient.NewTransport(), Timeout: 10 * time.Second}
	}
	if parsed, err := url.Parse(p.config.IDPURI); err == nil && parsed.Scheme == "http" {
		logger.Warn("Using cas-auth idp_uri with no TLS is a security risk")
	}
	p.opts = p.buildSessionOptions()

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// MaterializeScopedSecrets admits the manifest-owned cookie secret for one
// immutable generation and exposes only its resolved-value descriptor.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}

	value, err := access.Materialize(ctx, "cookie.secret", p.config.Cookie.Secret)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}
	if err := value.Use(validateCookieSecret); err != nil {
		return secret.ErrCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return secret.ErrCredentialUnavailable
	}

	p.cookieSecret = value
	p.cookieSecretSet = true
	p.config.Cookie.Secret = descriptor.String()
	p.secretsPrepared = true
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Immutable generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}
	return secret.ErrCredentialUnavailable
}

func validateCookieSecret(plaintext string) error {
	if utf8.RuneCountInString(plaintext) < minCookieSecretLen {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.lifecycleMu.RLock()
		if p.retired || !p.secretsPrepared {
			p.lifecycleMu.RUnlock()
			http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == p.config.LogoutURI {
			p.logout(w, r)
			p.lifecycleMu.RUnlock()
			return
		}

		opts := p.sessionOptions()
		if sessionID := cookieValue(r, opts.cookieName); sessionID != "" {
			if p.refreshSession(sessionID) {
				p.lifecycleMu.RUnlock()
				next.ServeHTTP(w, r)
				return
			}
			p.deleteCookie(w, opts.cookieName)
			p.firstAccess(w, r)
			p.lifecycleMu.RUnlock()
			return
		}

		if r.Method == http.MethodGet && r.URL.Path == base.CallbackPath(p.config.CASCallbackURI) {
			apisixctx.RegisterSensitiveQueryName(r, "ticket")
			if ticket := r.URL.Query().Get("ticket"); ticket != "" {
				p.validateWithCAS(w, r, ticket)
				p.lifecycleMu.RUnlock()
				return
			}
		}

		if r.Method == http.MethodPost && r.URL.Path == base.CallbackPath(p.config.CASCallbackURI) {
			if !p.trustedLogoutPeer(r) {
				http.Error(w, util.BuildMessageResponse("untrusted logout request from IdP"), http.StatusForbidden)
				p.lifecycleMu.RUnlock()
				return
			}
			if p.handleIDPLogout(r) {
				w.WriteHeader(http.StatusOK)
				p.lifecycleMu.RUnlock()
				return
			}
			http.Error(
				w,
				util.BuildMessageResponse("invalid logout request from IdP, no ticket"),
				http.StatusBadRequest,
			)
			p.lifecycleMu.RUnlock()
			return
		}

		p.firstAccess(w, r)
		p.lifecycleMu.RUnlock()
	})
}

func (p *Plugin) handleIDPLogout(r *http.Request) bool {
	if r.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLogoutBodyBytes+1))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > maxLogoutBodyBytes {
		return false
	}

	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.Strict = true
	rootSeen := false
	rootClosed := false
	depth := 0
	sessionIndexCount := 0
	sessionIndexDepth := 0
	xmlDeclarationSeen := false
	var sessionIndex strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !rootSeen || !rootClosed || depth != 0 || sessionIndexCount != 1 {
				return false
			}
			sessionID := strings.TrimSpace(sessionIndex.String())
			if sessionID == "" {
				return false
			}
			p.deleteSession(sessionID)
			return true
		}
		if err != nil {
			return false
		}
		switch value := token.(type) {
		case xml.Directive:
			return false
		case xml.ProcInst:
			if rootSeen || xmlDeclarationSeen || !strings.EqualFold(value.Target, "xml") {
				return false
			}
			xmlDeclarationSeen = true
		case xml.Comment:
			if rootClosed {
				return false
			}
		case xml.StartElement:
			if rootClosed {
				return false
			}
			name := localXMLName(value.Name)
			if sessionIndexDepth != 0 && depth >= sessionIndexDepth {
				return false
			}
			if !rootSeen {
				if depth != 0 || name != "LogoutRequest" {
					return false
				}
				rootSeen = true
			} else if name == "SessionIndex" {
				if depth != 1 || sessionIndexCount != 0 {
					return false
				}
				sessionIndexCount++
				sessionIndexDepth = depth + 1
			}
			depth++
		case xml.CharData:
			if !rootSeen && strings.TrimSpace(string(value)) != "" {
				return false
			}
			if rootClosed && strings.TrimSpace(string(value)) != "" {
				return false
			}
			if sessionIndexDepth != 0 {
				sessionIndex.Write([]byte(value))
			}
		case xml.EndElement:
			if depth == 0 {
				return false
			}
			depth--
			if depth == 0 {
				if localXMLName(value.Name) != "LogoutRequest" {
					return false
				}
				rootClosed = true
			}
			if sessionIndexDepth != 0 && depth < sessionIndexDepth {
				sessionIndexDepth = 0
			}
		}
	}
}

func (p *Plugin) trustedLogoutPeer(r *http.Request) bool {
	if len(p.logoutTrustedNets) == 0 {
		return false
	}
	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, network := range p.logoutTrustedNets {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *Plugin) firstAccess(w http.ResponseWriter, r *http.Request) {
	originalURI := r.URL.RequestURI()
	signed, err := p.signRequestURILocked(originalURI)
	if err != nil {
		http.Error(w, util.BuildMessageResponse("credential unavailable"), http.StatusServiceUnavailable)
		return
	}
	p.setCookie(w, requestURICookie, signed)

	values := url.Values{}
	values.Set("service", p.serviceURL(r))
	http.Redirect(w, r, strings.TrimRight(p.config.IDPURI, "/")+"/login?"+values.Encode(), http.StatusFound)
}

func (p *Plugin) validateWithCAS(w http.ResponseWriter, r *http.Request, ticket string) {
	requestURI, _ := p.verifyRequestURILocked(cookieValue(r, requestURICookie))
	if !safeRedirect(requestURI) {
		http.Error(w, util.BuildMessageResponse("invalid callback state"), http.StatusUnauthorized)
		return
	}

	user, err := p.validateTicket(r, ticket)
	if err != nil || user == "" {
		http.Error(w, util.BuildMessageResponse("invalid ticket"), http.StatusUnauthorized)
		return
	}

	p.storeSession(ticket, user)
	p.setCookie(w, p.sessionOptions().cookieName, ticket)
	p.deleteCookie(w, requestURICookie)
	http.Redirect(w, r, requestURI, http.StatusFound)
}

func (p *Plugin) signRequestURILocked(originalURI string) (string, error) {
	var signed string
	err := p.useCookieSecretLocked(func(cookieSecret string) error {
		payload := []byte(originalURI)
		defer clear(payload)
		signed = base.SignRawSessionValue(payload, cookieSecret)
		return nil
	})
	return signed, err
}

func (p *Plugin) verifyRequestURILocked(signed string) (string, bool) {
	var decoded []byte
	err := p.useCookieSecretLocked(func(cookieSecret string) error {
		var ok bool
		decoded, ok = base.VerifyRawSessionValue(signed, cookieSecret, nil)
		if !ok {
			return secret.ErrCredentialUnavailable
		}
		return nil
	})
	if err != nil {
		clear(decoded)
		return "", false
	}
	requestURI := string(decoded)
	clear(decoded)
	return requestURI, true
}

func (p *Plugin) useCookieSecretLocked(use func(string) error) error {
	if use == nil || p.retired || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.cookieSecretSet {
		return p.cookieSecret.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) logout(w http.ResponseWriter, r *http.Request) {
	opts := p.sessionOptions()
	sessionID := cookieValue(r, opts.cookieName)
	if sessionID == "" {
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	}

	p.deleteSession(sessionID)

	p.deleteCookie(w, opts.cookieName)
	http.Redirect(w, r, strings.TrimRight(p.config.IDPURI, "/")+"/logout", http.StatusFound)
}

func (p *Plugin) validateTicket(r *http.Request, ticket string) (string, error) {
	values := url.Values{}
	values.Set("ticket", ticket)
	values.Set("service", p.serviceURL(r))

	req, err := http.NewRequestWithContext(
		r.Context(),
		http.MethodGet,
		strings.TrimRight(p.config.IDPURI, "/")+"/serviceValidate?"+values.Encode(),
		nil,
	)
	if err != nil {
		return "", err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CAS serviceValidate returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseCASUser(body), nil
}

func (p *Plugin) serviceURL(r *http.Request) string {
	if isAbsoluteCallback(p.config.CASCallbackURI) {
		return p.config.CASCallbackURI
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host, requestPort := splitRequestHost(r.Host)
	port := requestPort
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		if _, listenerPort, err := net.SplitHostPort(local.String()); err == nil {
			port = listenerPort
		}
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(host, port) + p.config.CASCallbackURI
}

func (p *Plugin) sessionOptions() sessionOptions {
	return p.opts
}

// buildSessionOptions derives the cookie name and fingerprint from the static
// configuration; called once at PostInit instead of on every request.
func (p *Plugin) buildSessionOptions() sessionOptions {
	fingerprint := base.Sha256Hex(p.config.IDPURI + "|" + p.config.CASCallbackURI)
	return sessionOptions{
		cookieName:  sessionPrefix + fingerprint,
		fingerprint: fingerprint,
	}
}

func (p *Plugin) storeSession(sessionID string, user string) {
	processSessions.put(p.sessionKey(sessionID), sessionEntry{
		fingerprint: p.sessionOptions().fingerprint,
		user:        user,
	})
}

func (p *Plugin) refreshSession(sessionID string) bool {
	return processSessions.refresh(p.sessionKey(sessionID), p.sessionOptions().fingerprint)
}

func (p *Plugin) deleteSession(sessionID string) {
	processSessions.remove(p.sessionKey(sessionID))
}

func (p *Plugin) sessionKey(sessionID string) string {
	return p.sessionOptions().fingerprint + ":" + sessionID
}

func (p *Plugin) setCookie(w http.ResponseWriter, name string, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   p.config.Cookie.Secure == nil || *p.config.Cookie.Secure,
		SameSite: sameSiteMode(p.config.Cookie.SameSite),
	})
}

func (p *Plugin) deleteCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "deleted",
		Path:     "/",
		HttpOnly: true,
		Secure:   p.config.Cookie.Secure == nil || *p.config.Cookie.Secure,
		SameSite: sameSiteMode(p.config.Cookie.SameSite),
		MaxAge:   -1,
	})
}

// Stop waits for in-flight CAS/session operations, closes the generation's
// idle client connections, and then retires its private secret owners. The
// process-wide CAS session cache is intentionally generation-neutral and is
// not cleared here.
func (p *Plugin) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.retired {
		return
	}
	p.retired = true
	if p.client != nil {
		p.client.CloseIdleConnections()
		p.client = nil
	}
	p.cookieSecret = secret.Value{}
	p.cookieSecretSet = false
	p.secretsPrepared = false
}

func parseCASUser(body []byte) string {
	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	inSuccess := false
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := localXMLName(value.Name)
			if name == "authenticationSuccess" {
				inSuccess = true
			}
			if inSuccess && name == "user" {
				var user string
				if err := decoder.DecodeElement(&user, &value); err != nil {
					return ""
				}
				return user
			}
		case xml.EndElement:
			if localXMLName(value.Name) == "authenticationSuccess" {
				inSuccess = false
			}
		}
	}
}

func isAbsoluteCallback(callbackURI string) bool {
	parsed, err := url.Parse(callbackURI)
	return err == nil && parsed.IsAbs()
}

func safeRedirect(uri string) bool {
	if uri == "" || !strings.HasPrefix(uri, "/") || strings.HasPrefix(uri, "//") {
		return false
	}
	return !strings.ContainsAny(uri, "\\\r\n")
}

func cookieValue(r *http.Request, name string) string {
	cookie, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func splitRequestHost(hostport string) (string, string) {
	host, port, err := net.SplitHostPort(hostport)
	if err == nil {
		return host, port
	}
	return strings.Trim(hostport, "[]"), ""
}

func sameSiteMode(value string) http.SameSite {
	switch value {
	case "None":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

func localXMLName(name xml.Name) string {
	if name.Local != "" {
		return name.Local
	}
	return name.Space
}
