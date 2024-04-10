package key_auth

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2500
	name     = "key-auth"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "header": {
		"type": "string",
		"default": "apikey"
	  },
	  "query": {
		"type": "string",
		"default": "apikey"
	  },
	  "hide_credentials": {
		"type": "boolean",
		"default": false
	  }
	}
}`

type Config struct {
	Header          string `json:"header"`
	Query           string `json:"query"`
	HideCredentials *bool  `json:"hide_credentials"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Header == "" {
		p.config.Header = "apikey"
	}

	if p.config.Query == "" {
		p.config.Query = "apikey"
	}

	if p.config.HideCredentials == nil {
		hideCredentials := false
		p.config.HideCredentials = &hideCredentials
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

type basicAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		fromHeader := true
		key := r.Header.Get(p.config.Header)
		if key == "" {
			key = r.URL.Query().Get(p.config.Query)
			fromHeader = false
		}

		if key == "" {
			http.Error(w, `{"message": "Missing API key in request"}`, http.StatusUnauthorized)
			return
		}

		// note: here it's  unique key => consumer, it's different from basic-auth
		consumer, err := store.GetConsumerByPluginKey(name, key)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, `{"message": "Missing related consumer"}`, http.StatusUnauthorized)
			return
		}

		fmt.Printf("the consumer is %+v\n", consumer)

		if *p.config.HideCredentials {
			if fromHeader {
				r.Header.Del(p.config.Header)
			} else {
				r.URL.Query().Del(p.config.Query)
			}
		}

		// FIXME: attach current_consumer

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
