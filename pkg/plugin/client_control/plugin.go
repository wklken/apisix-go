package client_control

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 22000
	name     = "client-control"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "max_body_size": {
		"type": "integer",
		"minimum": 0,
		"description": "Maximum message body size in bytes. No restriction when set to 0."
	  }
	}
}`

type Config struct {
	MaxBodySize int64 `json:"max_body_size"`
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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.MaxBodySize > 0 {
			body, err := readLimitedBody(w, r, p.config.MaxBodySize)
			if err != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					if isChunkedRequest(r) {
						logger.Error("client intended to send too large chunked body")
					}
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				logger.Errorf("read request body fail: %s", err)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}

			// reset the r.Body
			r.Body = io.NopCloser(bytes.NewReader(body))

			next.ServeHTTP(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

// readLimitedBody bounds the read at max bytes and classifies oversized
// bodies through the typed *http.MaxBytesError.
func readLimitedBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	return io.ReadAll(r.Body)
}

func isChunkedRequest(r *http.Request) bool {
	for _, encoding := range r.TransferEncoding {
		if strings.EqualFold(encoding, "chunked") {
			return true
		}
	}
	return strings.EqualFold(r.Header.Get("Transfer-Encoding"), "chunked")
}
