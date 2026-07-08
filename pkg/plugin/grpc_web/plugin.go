package grpc_web

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 505
	name     = "grpc-web"

	defaultCorsAllowOrigin   = "*"
	defaultCorsAllowMethods  = http.MethodPost
	defaultCorsAllowHeaders  = "content-type,x-grpc-web,x-user-agent"
	defaultCorsExposeHeaders = "grpc-message,grpc-status"
	defaultProxyContentType  = "application/grpc"

	encodingBinary = "binary"
	encodingBase64 = "base64"
)

const schema = `
{
  "type": "object",
  "properties": {
    "cors_allow_headers": {
      "type": "string",
      "default": "content-type,x-grpc-web,x-user-agent"
    }
  }
}
`

var grpcWebContentEncodings = map[string]string{
	"application/grpc-web":            encodingBinary,
	"application/grpc-web-text":       encodingBase64,
	"application/grpc-web+proto":      encodingBinary,
	"application/grpc-web-text+proto": encodingBase64,
}

type Config struct {
	CorsAllowHeaders string `json:"cors_allow_headers,omitempty"`
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.CorsAllowHeaders == "" {
		p.config.CorsAllowHeaders = defaultCorsAllowHeaders
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		p.setCommonCorsHeaders(w.Header())

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", defaultCorsAllowMethods)
			w.Header().Set("Access-Control-Allow-Headers", p.config.CorsAllowHeaders)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mime := r.Header.Get("Content-Type")
		encoding, ok := grpcWebContentEncodings[mime]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := rewriteGRPCPath(r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := transformRequest(r, encoding); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		p.transformResponse(recorder, mime, encoding)
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func rewriteGRPCPath(r *http.Request) error {
	path, ok := wildcardParam(r)
	if !ok {
		if chi.RouteContext(r.Context()) == nil {
			return nil
		}
		return fmt.Errorf("grpc-web plugin requires prefix wildcard route")
	}

	if path == "" {
		path = "/"
	} else if path[0] != '/' {
		path = "/" + path
	}
	r.URL.Path = path
	return nil
}

func wildcardParam(r *http.Request) (string, bool) {
	rctx := chi.RouteContext(r.Context())
	if rctx == nil {
		return "", false
	}
	for i := len(rctx.URLParams.Keys) - 1; i >= 0; i-- {
		if rctx.URLParams.Keys[i] == "*" && len(rctx.URLParams.Values) > i {
			return rctx.URLParams.Values[i], true
		}
	}
	return "", false
}

func transformRequest(r *http.Request, encoding string) error {
	if encoding == encodingBase64 {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(string(body))
		if err != nil {
			return fmt.Errorf("failed to decode grpc-web request body: %w", err)
		}
		bodyReader := bytes.NewReader(decoded)
		r.Body = io.NopCloser(bodyReader)
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(decoded)), nil
		}
		r.ContentLength = int64(len(decoded))
		r.Header.Set("Content-Length", fmt.Sprint(len(decoded)))
	}

	r.Header.Set("Content-Type", defaultProxyContentType)
	r.Header.Set("TE", "trailers")
	return nil
}

func (p *Plugin) transformResponse(resp *responseRecorder, mime string, encoding string) {
	p.setCommonCorsHeaders(resp.header)
	resp.header.Set("Content-Type", mime)
	resp.header.Del("Content-Length")

	status := resp.header.Get("Grpc-Status")
	message := resp.header.Get("Grpc-Message")
	if status == "" {
		status = "2"
		message = "upstream grpc status not received"
	}

	trailer := buildTrailer(status, message)
	if encoding == encodingBase64 {
		encodedBody := base64.StdEncoding.EncodeToString(resp.body.Bytes())
		encodedTrailer := base64.StdEncoding.EncodeToString(trailer)
		resp.body.Reset()
		_, _ = resp.body.WriteString(encodedBody + encodedTrailer)
		return
	}

	_, _ = resp.body.Write(trailer)
}

func (p *Plugin) setCommonCorsHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", defaultCorsAllowOrigin)
	header.Set("Access-Control-Expose-Headers", defaultCorsExposeHeaders)
}

func buildTrailer(status string, message string) []byte {
	trailer := []byte("grpc-status:" + status + "\r\n" + "grpc-message:" + message + "\r\n")
	size := len(trailer)
	out := []byte{
		0x80,
		byte(size >> 24),
		byte(size >> 16),
		byte(size >> 8),
		byte(size),
	}
	return append(out, trailer...)
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
	_, _ = w.Write(r.body.Bytes())
}
