package ldap_auth

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
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
      "default": "ldap"
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
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid authorization in request")
			return
		}

		if err := p.authenticate(user.username, user.password, p.config); err != nil {
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
			return
		}

		consumer, err := store.GetConsumerByPluginKey(name, p.userDN(user.username))
		if err != nil {
			p.writeAuthError(w, http.StatusUnauthorized, "Invalid user authorization")
			return
		}

		ctx.AttachConsumer(r, consumer)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

type basicUser struct {
	username string
	password string
}

var (
	errMissingAuthorization = fmt.Errorf("missing authorization")
	errInvalidAuthorization = fmt.Errorf("invalid authorization")
)

func extractBasicUser(authorization string) (basicUser, error) {
	if authorization == "" {
		return basicUser{}, errMissingAuthorization
	}

	const prefix = "basic "
	if !strings.HasPrefix(strings.ToLower(authorization), prefix) {
		return basicUser{}, errInvalidAuthorization
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authorization[len(prefix):]))
	if err != nil {
		return basicUser{}, errInvalidAuthorization
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return basicUser{}, errInvalidAuthorization
	}

	username := removeWhitespace(parts[0])
	password := removeWhitespace(parts[1])
	if username == "" {
		return basicUser{}, errInvalidAuthorization
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
	http.Error(w, util.BuildMessageResponse(message), status)
}

func defaultLDAPAuthenticate(username, password string, cfg Config) error {
	conn, err := ldap.DialURL(ldapDialURL(cfg), ldap.DialWithTLSConfig(&tls.Config{
		InsecureSkipVerify: !cfg.TLSVerify,
	}))
	if err != nil {
		return err
	}
	defer conn.Close()

	userDN := cfg.UID + "=" + username + "," + cfg.BaseDN
	return conn.Bind(userDN, password)
}

func ldapDialURL(cfg Config) string {
	address := strings.TrimPrefix(strings.TrimPrefix(cfg.LDAPURI, "ldap://"), "ldaps://")
	if cfg.UseTLS {
		return "ldaps://" + address
	}
	return "ldap://" + address
}
