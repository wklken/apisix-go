package key_auth

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	  },
	  "anonymous_consumer": {
		"type": "string",
		"minLength": 1
	  }
	}
}`

type Config struct {
	Header            string `json:"header"`
	Query             string `json:"query"`
	HideCredentials   *bool  `json:"hide_credentials"`
	AnonymousConsumer string `json:"anonymous_consumer,omitempty"`
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

func (p *Plugin) Config() any {
	return &p.config
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
			if p.attachAnonymousConsumer(w, r, next) {
				return
			}
			writeAuthError(w, http.StatusUnauthorized, `{"message":"Missing API key in request"}`)
			return
		}

		// note: here it's  unique key => consumer, it's different from basic-auth
		consumer, err := store.GetConsumerByPluginKey(name, key)
		if errors.Is(err, store.ErrNotFound) {
			if p.config.AnonymousConsumer != "" {
				p.hideAllCredentials(r)
				if p.attachAnonymousConsumer(w, r, next) {
					return
				}
			}
			writeAuthError(w, http.StatusUnauthorized, `{"message":"Invalid API key in request"}`)
			return
		}

		if *p.config.HideCredentials {
			if fromHeader {
				r.Header.Del(p.config.Header)
			} else {
				query := r.URL.Query()
				query.Del(p.config.Query)
				r.URL.RawQuery = query.Encode()
			}
		}

		ctx.AttachConsumer(r, consumer)
		ctx.RunConsumerPlugins(w, r, next)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) attachAnonymousConsumer(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	if p.config.AnonymousConsumer == "" {
		return false
	}

	consumer, err := store.GetConsumer(p.config.AnonymousConsumer)
	if err != nil {
		ctx.RecordAuthProbeDiagnostic(r, fmt.Sprintf("failed to get anonymous consumer %s", p.config.AnonymousConsumer))
		writeAuthError(w, http.StatusUnauthorized, `{"message":"Invalid user authorization"}`)
		return true
	}

	ctx.AttachConsumer(r, consumer)
	ctx.RunConsumerPlugins(w, r, next)
	return true
}

func writeAuthError(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (p *Plugin) hideAllCredentials(r *http.Request) {
	if !*p.config.HideCredentials {
		return
	}

	r.Header.Del(p.config.Header)
	query := r.URL.Query()
	query.Del(p.config.Query)
	r.URL.RawQuery = query.Encode()
}
