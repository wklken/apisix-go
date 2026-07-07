package multi_auth

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/hmac_auth"
	"github.com/wklken/apisix-go/pkg/plugin/jwt_auth"
	"github.com/wklken/apisix-go/pkg/plugin/key_auth"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
	auths  []configuredAuth
}

const (
	priority = 2600
	name     = "multi-auth"
)

const schema = `
{
  "type": "object",
  "title": "work with route or service object",
  "properties": {
    "auth_plugins": {
      "type": "array",
      "minItems": 2
    }
  },
  "required": ["auth_plugins"]
}
`

type Config struct {
	AuthPlugins []AuthPluginConfig `json:"auth_plugins"`
}

type AuthPluginConfig map[string]map[string]any

type authPlugin interface {
	Init() error
	PostInit() error
	Config() interface{}
	GetSchema() string
	Handler(http.Handler) http.Handler
}

type configuredAuth struct {
	name   string
	plugin authPlugin
}

type probeResponseWriter struct {
	header http.Header
	status int
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.AuthPlugins) < 2 {
		return fmt.Errorf("auth_plugins must contain at least two auth plugins")
	}

	p.auths = make([]configuredAuth, 0, len(p.config.AuthPlugins))
	for _, authPlugin := range p.config.AuthPlugins {
		for authName, authConfig := range authPlugin {
			auth, err := newAuthPlugin(authName)
			if err != nil {
				return err
			}
			if err := auth.Init(); err != nil {
				return err
			}
			if err := util.Validate(authConfig, auth.GetSchema()); err != nil {
				return fmt.Errorf("plugin %s check schema failed: %w", authName, err)
			}
			if err := util.Parse(authConfig, auth.Config()); err != nil {
				return fmt.Errorf("plugin %s parse config failed: %w", authName, err)
			}
			if err := auth.PostInit(); err != nil {
				return err
			}
			p.auths = append(p.auths, configuredAuth{name: authName, plugin: auth})
		}
	}
	if len(p.auths) < 2 {
		return fmt.Errorf("auth_plugins must contain at least two auth plugins")
	}
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, auth := range p.auths {
			if auth.succeeds(r) {
				next.ServeHTTP(w, r)
				return
			}
		}

		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Authorization Failed"}`))
	})
}

func (a configuredAuth) succeeds(r *http.Request) bool {
	called := false
	probeNext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	writer := &probeResponseWriter{header: http.Header{}, status: http.StatusOK}
	a.plugin.Handler(probeNext).ServeHTTP(writer, r)
	return called
}

func newAuthPlugin(name string) (authPlugin, error) {
	switch name {
	case "basic-auth":
		return &basic_auth.Plugin{}, nil
	case "key-auth":
		return &key_auth.Plugin{}, nil
	case "jwt-auth":
		return &jwt_auth.Plugin{}, nil
	case "hmac-auth":
		return &hmac_auth.Plugin{}, nil
	default:
		return nil, fmt.Errorf("%s plugin is not supported", name)
	}
}

func (w *probeResponseWriter) Header() http.Header {
	return w.header
}

func (w *probeResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func (w *probeResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(body), nil
}

var _ http.ResponseWriter = (*probeResponseWriter)(nil)
