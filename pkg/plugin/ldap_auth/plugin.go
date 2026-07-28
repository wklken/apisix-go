package ldap_auth

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config       Config
	authenticate ldapAuthenticator
}

type ldapAuthenticator func(username, password string, cfg Config) error

const (
	priority = 2540
	name     = "ldap-auth"
)

const schema = `
{
  "type": "object",
  "title": "work with route or service object",
  "properties": {
    "base_dn": {
      "type": "string"
    },
    "ldap_uri": {
      "type": "string"
    },
    "use_tls": {
      "type": "boolean",
      "default": false
    },
    "tls_verify": {
      "type": "boolean",
      "default": false
    },
    "uid": {
      "type": "string",
      "default": "cn"
    },
    "realm": {
      "type": "string",
      "pattern": "^[\\x20-\\x21\\x23-\\x5B\\x5D-\\x7E]+$",
      "default": "ldap",
      "minLength": 1,
      "maxLength": 128
    }
  },
  "required": ["base_dn", "ldap_uri"]
}
`

type Config struct {
	BaseDN    string `json:"base_dn"`
	LDAPURI   string `json:"ldap_uri"`
	UseTLS    bool   `json:"use_tls,omitempty"`
	TLSVerify bool   `json:"tls_verify,omitempty"`
	UID       string `json:"uid,omitempty"`
	Realm     string `json:"realm,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.UID == "" {
		p.config.UID = "cn"
	}
	if p.config.Realm == "" {
		p.config.Realm = "ldap"
	}
	if p.authenticate == nil {
		p.authenticate = defaultLDAPAuthenticate
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user, err := extractBasicUser(r.Header.Get("Authorization"))
		if err != nil {
			if err == errMissingAuthorization {
				p.writeAuthError(w, http.StatusUnauthorized, "Missing authorization in request")
				return
			}
			p.recordAuthDiagnostic(r, err.Error())
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid authorization in request")
			return
		}

		if err := p.authenticate(user.username, user.password, p.config); err != nil {
			p.recordAuthDiagnostic(r, "ldap-auth failed: "+err.Error())
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
			return
		}

		consumer, err := store.GetConsumerByPluginKey(name, p.userDN(user.username))
		if err != nil {
			p.recordAuthDiagnostic(r, "failed to find user: invalid user")
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
			return
		}
		logger.Info("find consumer " + consumer.Username)

		ctx.AttachConsumer(r, consumer)
		ctx.RunConsumerPlugins(w, r, next)
	}
	return http.HandlerFunc(fn)
}

type basicUser struct {
	username string
	password string
}

var errMissingAuthorization = fmt.Errorf("missing authorization")

type authorizationError string

func (e authorizationError) Error() string {
	return string(e)
}

func extractBasicUser(authorization string) (basicUser, error) {
	if authorization == "" {
		return basicUser{}, errMissingAuthorization
	}

	scheme, encoded, found := strings.Cut(authorization, " ")
	if !found || !strings.EqualFold(scheme, "basic") || encoded == "" {
		return basicUser{}, authorizationError("Invalid authorization header format")
	}

	encoded = strings.TrimSpace(encoded)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return basicUser{}, authorizationError("Failed to decode authentication header: " + encoded)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return basicUser{}, authorizationError("Split authorization err: invalid decoded data: " + string(decoded))
	}

	username := removeWhitespace(parts[0])
	password := removeWhitespace(parts[1])
	if username == "" {
		return basicUser{}, authorizationError("Invalid authorization header format")
	}

	return basicUser{username: username, password: password}, nil
}

func removeWhitespace(value string) string {
	return strings.Join(strings.Fields(value), "")
}

func (p *Plugin) userDN(username string) string {
	return p.config.UID + "=" + username + "," + p.config.BaseDN
}

func (p *Plugin) writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+p.config.Realm+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(util.BuildMessageResponse(message)))
}

func (p *Plugin) recordAuthDiagnostic(r *http.Request, message string) {
	if !ctx.RecordAuthProbeDiagnostic(r, message) {
		logger.Warn(message)
	}
}

func defaultLDAPAuthenticate(username, password string, cfg Config) error {
	tlsConfig, err := ldapTLSConfig(cfg)
	if err != nil {
		return err
	}
	conn, err := ldap.DialURL(ldapDialURL(cfg), ldap.DialWithTLSConfig(tlsConfig))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	userDN := cfg.UID + "=" + username + "," + cfg.BaseDN
	return conn.Bind(userDN, password)
}

func ldapTLSConfig(cfg Config) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: !cfg.TLSVerify, //nolint:gosec // APISIX tls_verify explicitly controls LDAP verification
	}
	if !cfg.TLSVerify {
		return tlsConfig, nil
	}
	caFile := os.Getenv("SSL_CERT_FILE")
	if caFile == "" {
		return tlsConfig, nil
	}
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read LDAP trusted certificate file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, fmt.Errorf("parse LDAP trusted certificate file %q", caFile)
	}
	tlsConfig.RootCAs = roots
	return tlsConfig, nil
}

func ldapDialURL(cfg Config) string {
	address := strings.TrimPrefix(strings.TrimPrefix(cfg.LDAPURI, "ldap://"), "ldaps://")
	if cfg.UseTLS {
		return "ldaps://" + address
	}
	return "ldap://" + address
}
