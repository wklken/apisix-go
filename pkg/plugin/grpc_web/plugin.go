package grpc_web

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"slices"
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

func (p *Plugin) Config() any {
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
		if r.Method == http.MethodOptions {
			p.setCommonCorsHeaders(w.Header())
			w.Header().Set("Access-Control-Allow-Methods", defaultCorsAllowMethods)
			w.Header().Set("Access-Control-Allow-Headers", p.config.CorsAllowHeaders)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			p.setCommonCorsHeaders(w.Header())
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mime := r.Header.Get("Content-Type")
		encoding, ok := grpcWebContentEncodings[mime]
		if !ok {
			p.setCommonCorsHeaders(w.Header())
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := rewriteGRPCPath(r); err != nil {
			p.setCommonCorsHeaders(w.Header())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := transformRequest(r, encoding); err != nil {
			p.setCommonCorsHeaders(w.Header())
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		stream := newStreamingResponseWriter(w, mime, encoding, p.setCommonCorsHeaders)
		next.ServeHTTP(stream, r)
		p.setCommonCorsHeaders(stream.Header())
		_ = stream.finish()
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
	for i := range slices.Backward(rctx.URLParams.Keys) {
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

type streamingResponseWriter struct {
	writer      http.ResponseWriter
	mime        string
	encoding    string
	ensure      func(http.Header)
	wroteHeader bool
	wroteBody   bool
}

func newStreamingResponseWriter(
	writer http.ResponseWriter,
	mime string,
	encoding string,
	ensure func(http.Header),
) *streamingResponseWriter {
	writer.Header().Set("Content-Type", mime)
	writer.Header().Del("Content-Length")
	return &streamingResponseWriter{
		writer:   writer,
		mime:     mime,
		encoding: encoding,
		ensure:   ensure,
	}
}

func (w *streamingResponseWriter) Header() http.Header {
	return w.writer.Header()
}

func (w *streamingResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	if w.ensure != nil {
		w.ensure(w.writer.Header())
	}
	w.writer.Header().Set("Content-Type", w.mime)
	w.writer.Header().Del("Content-Length")
	w.writer.WriteHeader(statusCode)
	w.wroteHeader = true
}

func (w *streamingResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if len(body) == 0 {
		return 0, nil
	}
	w.wroteBody = true
	if w.encoding == encodingBase64 {
		encoded := base64.StdEncoding.EncodeToString(body)
		if _, err := io.WriteString(w.writer, encoded); err != nil {
			return 0, err
		}
		w.Flush()
		return len(body), nil
	}
	n, err := w.writer.Write(body)
	w.Flush()
	return n, err
}

func (w *streamingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.writer).Flush()
}

func (w *streamingResponseWriter) finish() error {
	promoteGRPCTrailerMetadata(w.Header())
	status := w.Header().Get("Grpc-Status")
	message := w.Header().Get("Grpc-Message")
	if !w.wroteBody && status != "" {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		return nil
	}
	if status == "" {
		status = "2"
		message = "upstream grpc status not received"
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	trailer := buildTrailer(status, message)
	if w.encoding == encodingBase64 {
		_, err := io.WriteString(w.writer, base64.StdEncoding.EncodeToString(trailer))
		w.Flush()
		return err
	}
	_, err := w.writer.Write(trailer)
	w.Flush()
	return err
}

func promoteGRPCTrailerMetadata(header http.Header) {
	for _, field := range []string{"Grpc-Status", "Grpc-Message"} {
		if header.Get(field) == "" {
			if value := trailerHeaderValue(header, field); value != "" {
				header.Set(field, value)
			}
		}
		deleteTrailerHeader(header, field)
	}
	removeGRPCTrailerAnnouncement(header)
}

func trailerHeaderValue(header http.Header, field string) string {
	want := http.TrailerPrefix + field
	for key, values := range header {
		if !strings.EqualFold(key, want) || len(values) == 0 {
			continue
		}
		return values[0]
	}
	return ""
}

func deleteTrailerHeader(header http.Header, field string) {
	want := http.TrailerPrefix + field
	for key := range header {
		if strings.EqualFold(key, want) {
			delete(header, key)
		}
	}
}

func removeGRPCTrailerAnnouncement(header http.Header) {
	for key, values := range header {
		if !strings.EqualFold(key, "Trailer") {
			continue
		}
		remaining := make([]string, 0, len(values))
		for _, value := range values {
			for token := range strings.SplitSeq(value, ",") {
				token = strings.TrimSpace(token)
				if token == "" || strings.EqualFold(token, "Grpc-Status") ||
					strings.EqualFold(token, "Grpc-Message") {
					continue
				}
				remaining = append(remaining, token)
			}
		}
		if len(remaining) == 0 {
			delete(header, key)
			continue
		}
		header[key] = []string{strings.Join(remaining, ", ")}
	}
}

func (p *Plugin) setCommonCorsHeaders(header http.Header) {
	if header.Get("Access-Control-Allow-Origin") == "" {
		header.Set("Access-Control-Allow-Origin", defaultCorsAllowOrigin)
	}
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
