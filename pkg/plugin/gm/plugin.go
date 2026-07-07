package gm

import (
	"errors"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = -43
	name     = "gm"
)

const schema = `
{
  "type": "object",
  "properties": {}
}
`

type Config struct{}

type SSLConfig struct {
	Cert  string   `json:"cert,omitempty"`
	Key   string   `json:"key,omitempty"`
	Certs []string `json:"certs,omitempty"`
	Keys  []string `json:"keys,omitempty"`
	GM    bool     `json:"gm,omitempty"`
	SNIs  []string `json:"snis,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func ValidateSSLConfig(cfg SSLConfig) error {
	if !cfg.GM {
		return nil
	}
	if cfg.Cert == "" || cfg.Key == "" {
		return errors.New("enc cert/key are required")
	}
	if len(cfg.Certs) != 1 || len(cfg.Keys) != 1 {
		return errors.New("sign cert/key are required")
	}
	return nil
}
