package proxy_mirror

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
}

const (
	priority = 1010
	name     = "proxy-mirror"
)

const schema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string"
    },
    "path": {
      "type": "string"
    },
    "path_concat_mode": {
      "type": "string",
      "default": "replace",
      "enum": ["replace", "prefix"]
    },
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1,
      "default": 1
    }
  },
  "required": ["host"]
}
`

type Config struct {
	Host           string  `json:"host"`
	Path           string  `json:"path,omitempty"`
	PathConcatMode string  `json:"path_concat_mode,omitempty"`
	SampleRatio    float64 `json:"sample_ratio,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.PathConcatMode == "" {
		p.config.PathConcatMode = "replace"
	}
	if p.config.SampleRatio == 0 {
		p.config.SampleRatio = 1
	}
	p.client = &http.Client{
		Timeout: 5 * time.Second,
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndRestoreBody(r)
		if err == nil && p.shouldMirror() {
			mirrorReq, err := p.buildMirrorRequest(r, body)
			if err == nil {
				go p.sendMirror(mirrorReq)
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func (p *Plugin) shouldMirror() bool {
	if p.config.SampleRatio >= 1 {
		return true
	}
	return rand.Float64() < p.config.SampleRatio
}

func (p *Plugin) buildMirrorRequest(r *http.Request, body []byte) (*http.Request, error) {
	target, err := p.mirrorURL(r)
	if err != nil {
		return nil, err
	}

	mirrorReq, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	mirrorReq.Header = r.Header.Clone()
	mirrorReq.Host = mirrorReq.URL.Host

	return mirrorReq, nil
}

func (p *Plugin) mirrorURL(r *http.Request) (string, error) {
	hostURL, err := url.Parse(p.config.Host)
	if err != nil {
		return "", err
	}

	mirrorPath := r.URL.Path
	if p.config.Path != "" {
		if p.config.PathConcatMode == "prefix" {
			mirrorPath = strings.TrimRight(p.config.Path, "/") + "/" + strings.TrimLeft(r.URL.Path, "/")
		} else {
			mirrorPath = p.config.Path
		}
	}

	hostURL.Path = mirrorPath
	hostURL.RawQuery = r.URL.RawQuery
	return hostURL.String(), nil
}

func (p *Plugin) sendMirror(req *http.Request) {
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
}
