package brotli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	brotlienc "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 996
	name     = "brotli"
)

const schema = `
{
  "type": "object",
  "properties": {
    "types": {
      "anyOf": [
        {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "string",
            "minLength": 1
          }
        },
        {
          "enum": ["*"]
        }
      ],
      "default": ["text/html"]
    },
    "min_length": {
      "type": "integer",
      "minimum": 1,
      "default": 20
    },
    "mode": {
      "type": "integer",
      "minimum": 0,
      "maximum": 2,
      "default": 0
    },
    "comp_level": {
      "type": "integer",
      "minimum": 0,
      "maximum": 11,
      "default": 6
    },
    "lgwin": {
      "type": "integer",
      "enum": [0,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24],
      "default": 19
    },
    "lgblock": {
      "type": "integer",
      "enum": [0,16,17,18,19,20,21,22,23,24],
      "default": 0
    },
    "http_version": {
      "enum": [1.1, 1.0],
      "default": 1.1
    },
    "vary": {
      "type": "boolean"
    }
  }
}
`

type Config struct {
	Types       []string `json:"types,omitempty"`
	MinLength   *int     `json:"min_length,omitempty"`
	Mode        *int     `json:"mode,omitempty"`
	CompLevel   *int     `json:"comp_level,omitempty"`
	LGWin       *int     `json:"lgwin,omitempty"`
	LGBlock     *int     `json:"lgblock,omitempty"`
	HTTPVersion *float64 `json:"http_version,omitempty"`
	Vary        *bool    `json:"vary,omitempty"`

	contentTypes map[string]struct{}
	wildcardType bool
	httpVersion  string
}

func (c *Config) UnmarshalJSON(data []byte) error {
	var raw struct {
		Types       json.RawMessage `json:"types"`
		MinLength   *int            `json:"min_length"`
		Mode        *int            `json:"mode"`
		CompLevel   *int            `json:"comp_level"`
		LGWin       *int            `json:"lgwin"`
		LGBlock     *int            `json:"lgblock"`
		HTTPVersion *float64        `json:"http_version"`
		Vary        *bool           `json:"vary"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*c = Config{
		MinLength:   raw.MinLength,
		Mode:        raw.Mode,
		CompLevel:   raw.CompLevel,
		LGWin:       raw.LGWin,
		LGBlock:     raw.LGBlock,
		HTTPVersion: raw.HTTPVersion,
		Vary:        raw.Vary,
	}
	if len(raw.Types) == 0 || string(raw.Types) == "null" {
		return nil
	}
	var wildcard string
	if err := json.Unmarshal(raw.Types, &wildcard); err == nil {
		c.Types = []string{wildcard}
		return nil
	}
	return json.Unmarshal(raw.Types, &c.Types)
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.Types) == 0 {
		p.config.Types = []string{"text/html"}
	}
	if p.config.MinLength == nil {
		value := 20
		p.config.MinLength = &value
	}
	if p.config.Mode == nil {
		value := 0
		p.config.Mode = &value
	}
	if p.config.CompLevel == nil {
		value := 6
		p.config.CompLevel = &value
	}
	if p.config.LGWin == nil {
		value := 19
		p.config.LGWin = &value
	}
	if p.config.LGBlock == nil {
		value := 0
		p.config.LGBlock = &value
	}
	if p.config.HTTPVersion == nil {
		value := 1.1
		p.config.HTTPVersion = &value
	}
	p.config.httpVersion = fmt.Sprintf("%g", *p.config.HTTPVersion)
	p.config.contentTypes = make(map[string]struct{}, len(p.config.Types))
	for _, contentType := range p.config.Types {
		if contentType == "*" {
			p.config.wildcardType = true
			continue
		}
		p.config.contentTypes[contentType] = struct{}{}
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.shouldConsiderRequest(r) {
			next.ServeHTTP(w, r)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		if p.shouldCompressResponse(recorder) {
			p.compressResponse(recorder)
		}
		recorder.writeTo(w)
	})
}

func (p *Plugin) shouldConsiderRequest(r *http.Request) bool {
	if !acceptsBrotli(r.Header.Get("Accept-Encoding")) {
		return false
	}
	reqHTTPVersion := fmt.Sprintf("%d.%d", r.ProtoMajor, r.ProtoMinor)
	return reqHTTPVersion >= p.config.httpVersion
}

func acceptsBrotli(acceptEncoding string) bool {
	for part := range strings.SplitSeq(acceptEncoding, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		token, params, _ := strings.Cut(part, ";")
		token = strings.TrimSpace(token)
		if token != "br" && token != "*" {
			continue
		}
		if qualityIsZero(params) {
			return false
		}
		return true
	}
	return false
}

func qualityIsZero(params string) bool {
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || key != "q" {
			continue
		}
		quality, err := strconv.ParseFloat(value, 64)
		return err == nil && quality == 0
	}
	return false
}

func (p *Plugin) shouldCompressResponse(resp *responseRecorder) bool {
	if resp.header.Get("Content-Encoding") != "" {
		return false
	}
	contentType := resp.header.Get("Content-Type")
	if contentType == "" {
		return false
	}
	if semi := strings.Index(contentType, ";"); semi >= 0 {
		contentType = contentType[:semi]
	}
	if !p.config.wildcardType {
		if _, ok := p.config.contentTypes[contentType]; !ok {
			return false
		}
	}
	contentLength := resp.header.Get("Content-Length")
	if contentLength != "" {
		length, err := strconv.Atoi(contentLength)
		if err == nil && length < *p.config.MinLength {
			return false
		}
	}
	return true
}

func (p *Plugin) compressResponse(resp *responseRecorder) {
	var compressed bytes.Buffer
	writer := brotlienc.NewWriterOptions(&compressed, p.writerOptions())
	_, writeErr := writer.Write(resp.body.Bytes())
	closeErr := writer.Close()
	if writeErr != nil || closeErr != nil {
		return
	}

	resp.body.Reset()
	_, _ = resp.body.Write(compressed.Bytes())
	resp.header.Set("Content-Encoding", "br")
	resp.header.Del("Content-Length")
	if p.config.Vary != nil && *p.config.Vary {
		if vary := resp.header.Get("Vary"); vary != "" {
			resp.header.Set("Vary", vary+", Accept-Encoding")
		} else {
			resp.header.Set("Vary", "Accept-Encoding")
		}
	}
	weakenETag(resp.header)
}

func (p *Plugin) writerOptions() brotlienc.WriterOptions {
	return brotlienc.WriterOptions{
		Quality: *p.config.CompLevel,
		LGWin:   *p.config.LGWin,
	}
}

func weakenETag(header http.Header) {
	etag := header.Get("Etag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		return
	}
	if len(etag) >= 2 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) {
		if strings.Contains(etag[1:len(etag)-1], `"`) {
			header.Del("Etag")
			return
		}
		header.Set("Etag", "W/"+etag)
		return
	}
	header.Del("Etag")
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		if strings.EqualFold(field, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = io.Copy(w, bytes.NewReader(r.body.Bytes()))
}
