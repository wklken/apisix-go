package brotli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	brotlienc "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
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

const (
	// defaultMaxResponseSize bounds the buffered compression input so a
	// large download cannot be double-buffered without limit.
	defaultMaxResponseSize = 10 * 1024 * 1024
)

type Config struct {
	Types       []string `json:"types,omitempty"`
	MinLength   *int     `json:"min_length,omitempty"`
	Mode        *int     `json:"mode,omitempty"`
	CompLevel   *int     `json:"comp_level,omitempty"`
	LGWin       *int     `json:"lgwin,omitempty"`
	LGBlock     *int     `json:"lgblock,omitempty"`
	HTTPVersion *float64 `json:"http_version,omitempty"`
	Vary        *bool    `json:"vary,omitempty"`

	contentTypes    map[string]struct{}
	wildcardType    bool
	httpVersion     string
	maxResponseSize int64
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
	if p.config.maxResponseSize <= 0 {
		p.config.maxResponseSize = defaultMaxResponseSize
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

// RegisterCompressionOffers exposes brotli through the shared request-local
// negotiation state. Eligibility is evaluated against the final response
// metadata supplied to compression.State.Decide.
func (p *Plugin) RegisterCompressionOffers(r *http.Request, _ *compression.State) []compression.Offer {
	return []compression.Offer{{
		Coding:   compression.Brotli,
		Rank:     996,
		Vary:     p.config.Vary != nil && *p.config.Vary,
		Eligible: p.requestEligible(r),
	}}
}

func (p *Plugin) WrapCompression(
	w http.ResponseWriter,
	_ *http.Request,
	_ *compression.State,
	decision compression.Decision,
) (http.ResponseWriter, error) {
	if decision.Coding != compression.Brotli {
		return w, nil
	}
	return newStreamingCompressionWriter(w, p.writerOptions()), nil
}

func (p *Plugin) RunStreamingHeaderFilter(_ *http.Request, _ *base.StreamingResponseState) error {
	return nil
}

func (p *Plugin) requestEligible(r *http.Request) func(compression.ResponseMeta) bool {
	return func(meta compression.ResponseMeta) bool {
		if r == nil || base.ProtocolVersion(r) < p.config.httpVersion {
			return false
		}
		return p.responseEligible(meta)
	}
}

type streamingCompressionWriter struct {
	http.ResponseWriter
	compressor  *brotlienc.Writer
	wroteHeader bool
	status      int
	hijacked    bool
	closeOnce   sync.Once
	closeErr    error
}

func newStreamingCompressionWriter(
	w http.ResponseWriter,
	options brotlienc.WriterOptions,
) *streamingCompressionWriter {
	return &streamingCompressionWriter{
		ResponseWriter: w,
		compressor:     brotlienc.NewWriterOptions(w, options),
	}
}

func (w *streamingCompressionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamingCompressionWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	if status >= 100 && status <= 199 || status == http.StatusNoContent ||
		status == http.StatusNotModified || status == http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if strings.TrimSpace(w.Header().Get("Content-Encoding")) != "" {
		w.compressor = nil
		w.ResponseWriter.WriteHeader(status)
		return
	}
	prepareBrotliHeaders(w.Header())
	w.Header().Set("Content-Encoding", "br")
	w.ResponseWriter.WriteHeader(status)
}

func (w *streamingCompressionWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.status == http.StatusNoContent || w.status == http.StatusNotModified ||
		w.status == http.StatusSwitchingProtocols || (w.status >= 100 && w.status <= 199) {
		return len(body), nil
	}
	if w.compressor == nil {
		return w.ResponseWriter.Write(body)
	}
	return w.compressor.Write(body)
}

func (w *streamingCompressionWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compressor != nil {
		_ = w.compressor.Flush()
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *streamingCompressionWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *streamingCompressionWriter) Close() error {
	w.closeOnce.Do(func() {
		if w.compressor != nil && !w.hijacked {
			closeErr := w.compressor.Close()
			flushErr := http.NewResponseController(w.ResponseWriter).Flush()
			if errors.Is(flushErr, http.ErrNotSupported) {
				flushErr = nil
			}
			w.closeErr = errors.Join(closeErr, flushErr)
		}
	})
	return w.closeErr
}

func (w *streamingCompressionWriter) FinishStreamingResponse(_ error) error {
	if !w.hijacked && !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.Close()
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if base.ProtocolVersion(r) < p.config.httpVersion {
			next.ServeHTTP(w, r)
			return
		}

		eligible := func(meta compression.ResponseMeta) bool {
			return p.responseEligible(meta)
		}
		r, state := compression.Register(r, compression.Offer{
			Coding:   compression.Brotli,
			Rank:     996,
			Vary:     p.config.Vary != nil && *p.config.Vary,
			Eligible: eligible,
		})
		bw := newBoundedResponseWriter(w, p.config.maxResponseSize)
		bw.requestMethod = r.Method
		bw.state = state
		next.ServeHTTP(bw, r)
		if bw.committed || bw.hijacked {
			// The response already streamed to the client; nothing to
			// rewrite and no second write may happen.
			return
		}

		recorder := bw.materialize()
		decision := state.Decide(compression.ResponseMeta{
			Method: r.Method,
			Status: recorder.StatusCode(),
			Header: recorder.Header().Clone(),
		})
		if decision.Vary {
			base.AppendVaryToken(recorder.Header(), "Accept-Encoding")
		}
		if decision.NotAcceptable {
			recorder.ReplaceBody(nil)
			recorder.SetStatusCode(http.StatusNotAcceptable)
		} else if decision.Coding == compression.Brotli && p.shouldCompressResponse(recorder) {
			if err := p.compressResponse(recorder); err != nil {
				logger.Errorf("brotli compress response fail: %s", err)
			}
		}
		writeCompressedResponse(w, recorder)
	})
}

// boundedResponseWriter buffers a response until either completion or the
// configured cap is exceeded. Once the cap is exceeded it switches once to
// pass-through: headers including Content-Length are copied, the status and
// the buffered bytes plus the current write chunk are flushed to the
// underlying writer, and later writes stream directly.
type boundedResponseWriter struct {
	base           http.ResponseWriter
	header         http.Header
	statusCode     int
	buffer         bytes.Buffer
	committed      bool
	cap            int64
	maxBuffered    int64
	requestMethod  string
	state          *compression.State
	bodySuppressed bool
	started        bool
	hijacked       bool
}

func newBoundedResponseWriter(base http.ResponseWriter, cap int64) *boundedResponseWriter {
	return &boundedResponseWriter{
		base:       base,
		header:     make(http.Header),
		statusCode: http.StatusOK,
		cap:        cap,
	}
}

func (w *boundedResponseWriter) Header() http.Header {
	return w.header
}

func (w *boundedResponseWriter) Unwrap() http.ResponseWriter {
	return w.base
}

func (w *boundedResponseWriter) Flush() {
	if !w.committed {
		w.commit(nil)
	}
	if flusher, ok := w.base.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *boundedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.base.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	if !w.committed && w.started {
		w.commit(nil)
	}
	conn, rw, err := hijacker.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}

func (w *boundedResponseWriter) WriteHeader(code int) {
	if w.committed {
		w.base.WriteHeader(code)
		return
	}
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		w.forwardInformational(code)
		return
	}
	if w.started {
		return
	}
	w.started = true
	w.statusCode = code
}

func (w *boundedResponseWriter) Write(p []byte) (int, error) {
	if w.committed {
		if w.bodySuppressed {
			return len(p), nil
		}
		return w.base.Write(p)
	}
	w.started = true
	if !base.ResponseAllowsBody(w.requestMethod, w.statusCode) {
		if strings.EqualFold(w.requestMethod, http.MethodHead) {
			return len(p), nil
		}
		return 0, http.ErrBodyNotAllowed
	}
	if w.cap > 0 && int64(w.buffer.Len())+int64(len(p)) > w.cap {
		w.commit(p)
		return len(p), nil
	}
	w.maxBuffered = max(w.maxBuffered, int64(w.buffer.Len())+int64(len(p)))
	_, err := w.buffer.Write(p)
	return len(p), err
}

func (w *boundedResponseWriter) commit(chunk []byte) {
	w.committed = true
	w.started = true
	decision := compression.Decision{Coding: compression.Identity}
	if w.state != nil {
		decision = w.state.Decide(compression.ResponseMeta{
			Method: w.requestMethod,
			Status: w.statusCode,
			Header: w.header.Clone(),
		})
	}
	if decision.Vary {
		base.AppendVaryToken(w.header, "Accept-Encoding")
	}
	identityForbiddenBrotliFallback := decision.Coding == compression.Brotli &&
		!decision.IdentityAllowed && headerValue(w.header, "Content-Encoding") == ""
	if decision.NotAcceptable || identityForbiddenBrotliFallback {
		w.bodySuppressed = true
		base.InvalidateBodyDerivedHeaders(w.header)
		w.replaceBaseHeaders()
		w.base.WriteHeader(http.StatusNotAcceptable)
		w.buffer.Reset()
		return
	}
	w.replaceBaseHeaders()
	w.base.WriteHeader(w.statusCode)
	if base.ResponseAllowsBody(w.requestMethod, w.statusCode) {
		if n := w.buffer.Len(); n > 0 {
			_, _ = w.base.Write(w.buffer.Bytes())
		}
		if len(chunk) > 0 {
			_, _ = w.base.Write(chunk)
		}
	}
	w.buffer.Reset()
}

func (w *boundedResponseWriter) forwardInformational(code int) {
	finalHeader := w.base.Header().Clone()
	replaceHeaders(w.base.Header(), w.header)
	w.base.WriteHeader(code)
	replaceHeaders(w.base.Header(), finalHeader)
}

func (w *boundedResponseWriter) replaceBaseHeaders() {
	replaceHeaders(w.base.Header(), w.header)
}

func replaceHeaders(dst, src http.Header) {
	for field := range dst {
		delete(dst, field)
	}
	for field, values := range src {
		dst[field] = append([]string(nil), values...)
	}
}

// materialize converts the still-buffered response into a
// BufferedResponseWriter so the compression pipeline can rewrite it.
func (w *boundedResponseWriter) materialize() *base.BufferedResponseWriter {
	recorder := base.GetOrCreateTransformResponseWriter(&http.Request{Method: w.requestMethod})
	for field, values := range w.header {
		for _, value := range values {
			recorder.Header().Add(field, value)
		}
	}
	recorder.WriteHeader(w.statusCode)
	recorder.SetBody(w.buffer.Bytes())
	return recorder
}

func (p *Plugin) shouldCompressResponse(resp *base.BufferedResponseWriter) bool {
	if resp.StatusCode() == http.StatusNotModified || resp.StatusCode() == http.StatusNoContent ||
		resp.StatusCode() == http.StatusSwitchingProtocols || (resp.StatusCode() >= 100 && resp.StatusCode() <= 199) {
		return false
	}
	if headerValue(resp.Header(), "Content-Encoding") != "" || !p.contentTypeEligible(resp.Header()) {
		return false
	}
	contentLength := resp.Header().Get("Content-Length")
	if contentLength != "" {
		length, err := strconv.Atoi(contentLength)
		if err == nil && length < *p.config.MinLength {
			return false
		}
	}
	return true
}

func (p *Plugin) responseEligible(meta compression.ResponseMeta) bool {
	if meta.Status == http.StatusNotModified {
		return p.contentTypeEligible(meta.Header)
	}
	if meta.Status == http.StatusSwitchingProtocols || meta.Status == http.StatusNoContent ||
		(meta.Status >= 100 && meta.Status <= 199) {
		return false
	}
	if !base.ResponseAllowsBody(meta.Method, meta.Status) && !strings.EqualFold(meta.Method, http.MethodHead) {
		return false
	}
	if headerValue(meta.Header, "Content-Encoding") != "" || !p.contentTypeEligible(meta.Header) {
		return false
	}
	if contentLength := headerValue(meta.Header, "Content-Length"); contentLength != "" {
		length, err := strconv.Atoi(strings.TrimSpace(contentLength))
		if err == nil && length < *p.config.MinLength {
			return false
		}
	}
	return true
}

func (p *Plugin) contentTypeEligible(header http.Header) bool {
	contentType := headerValue(header, "Content-Type")
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = contentType[:semi]
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if contentType == "" {
		return false
	}
	if p.config.wildcardType {
		return true
	}
	_, ok := p.config.contentTypes[contentType]
	return ok
}

func headerValue(header http.Header, name string) string {
	for actual, values := range header {
		if strings.EqualFold(actual, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// compressResponse replaces the buffered body with its brotli encoding. It
// returns a controlled error when the body exceeds the internal limit so
// oversized responses pass through uncompressed instead of growing without
// bound; below the cap headers and compression behavior are preserved.
func (p *Plugin) compressResponse(resp *base.BufferedResponseWriter) error {
	limit := p.config.maxResponseSize
	body := resp.Body()
	if limit > 0 && int64(len(body)) > limit {
		return fmt.Errorf(
			"response body of %d bytes exceeds internal limit %d",
			len(body),
			limit,
		)
	}
	var compressed bytes.Buffer
	writer := brotlienc.NewWriterOptions(&compressed, p.writerOptions())
	_, writeErr := writer.Write(body)
	closeErr := writer.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}

	resp.SetBody(compressed.Bytes())
	prepareBrotliHeaders(resp.Header())
	resp.Header().Set("Content-Encoding", "br")
	return nil
}

func prepareBrotliHeaders(header http.Header) {
	deleteHeader(header, "Content-Length")
	etag := headerValue(header, "Etag")
	if etag == "" {
		return
	}
	if strings.HasPrefix(etag, `W/"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 4 {
		return
	}
	if strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`) && len(etag) >= 2 {
		deleteHeader(header, "Etag")
		header.Set("Etag", "W/"+etag)
		return
	}
	deleteHeader(header, "Etag")
	logger.Errorf("no standard etag or regex match failed:")
}

func (p *Plugin) writerOptions() brotlienc.WriterOptions {
	return brotlienc.WriterOptions{
		Quality: *p.config.CompLevel,
		LGWin:   *p.config.LGWin,
	}
}

func writeCompressedResponse(w http.ResponseWriter, resp *base.BufferedResponseWriter) {
	brotli := strings.EqualFold(headerValue(resp.Header(), "Content-Encoding"), "br")
	for field, values := range resp.Header() {
		if brotli && strings.EqualFold(field, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.StatusCode())
	if brotli {
		_ = http.NewResponseController(w).Flush()
	}
	resp.WriteBodyTo(w)
}

func deleteHeader(header http.Header, name string) {
	for actual := range header {
		if strings.EqualFold(actual, name) {
			delete(header, actual)
		}
	}
}
