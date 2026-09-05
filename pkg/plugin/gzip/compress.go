package gzip

// we need to use the maybeCompressResponseWriter, not the middleware, so we need to copy the code here(it's not exported)

// This file is a copy of
// reference: https://github.com/Go-chi/chi/blob/v1.0.0/middleware/compress.go
// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Under the MIT License

import (
	"bufio"
	cgzip "compress/gzip"
	"compress/zlib"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
)

type encoding int

const (
	encodingNone encoding = iota
	encodingGzip
	encodingDeflate
)

var defaultContentTypes = map[string]struct{}{
	"text/html":                {},
	"text/css":                 {},
	"text/plain":               {},
	"text/javascript":          {},
	"application/javascript":   {},
	"application/x-javascript": {},
	"application/json":         {},
	"application/atom+xml":     {},
	"application/rss+xml ":     {},
}

type resettableWriteCloser interface {
	io.WriteCloser
	Reset(io.Writer)
}

var (
	gzipWriterPools    [10]sync.Pool
	deflateWriterPools [10]sync.Pool
)

func acquireCompressionWriter(enc encoding, level int, destination io.Writer) (resettableWriteCloser, error) {
	if level < 0 || level >= len(gzipWriterPools) {
		if enc == encodingGzip {
			return cgzip.NewWriterLevel(destination, level)
		}
		return zlib.NewWriterLevel(destination, level)
	}
	var pool *sync.Pool
	switch enc {
	case encodingGzip:
		pool = &gzipWriterPools[level]
	case encodingDeflate:
		pool = &deflateWriterPools[level]
	default:
		return nil, nil
	}
	if pooled := pool.Get(); pooled != nil {
		writer := pooled.(resettableWriteCloser)
		writer.Reset(destination)
		return writer, nil
	}
	if enc == encodingGzip {
		return cgzip.NewWriterLevel(destination, level)
	}
	return zlib.NewWriterLevel(destination, level)
}

func releaseCompressionWriter(enc encoding, level int, writer resettableWriteCloser) error {
	if writer == nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if level < 0 || level >= len(gzipWriterPools) {
		return nil
	}
	writer.Reset(io.Discard)
	switch enc {
	case encodingGzip:
		gzipWriterPools[level].Put(writer)
	case encodingDeflate:
		deflateWriterPools[level].Put(writer)
	}
	return nil
}

type maybeCompressResponseWriter struct {
	http.ResponseWriter
	w             io.Writer
	compressor    resettableWriteCloser
	encoding      encoding
	contentTypes  map[string]struct{}
	level         int
	wroteHeader   bool
	wildcardType  bool
	minLength     int
	requestMethod string
	status        int
	state         *compression.State
	hijacked      bool
}

var (
	_ http.ResponseWriter                       = (*maybeCompressResponseWriter)(nil)
	_ http.Flusher                              = (*maybeCompressResponseWriter)(nil)
	_ http.Hijacker                             = (*maybeCompressResponseWriter)(nil)
	_ interface{ Unwrap() http.ResponseWriter } = (*maybeCompressResponseWriter)(nil)
)

func (w *maybeCompressResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *maybeCompressResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *maybeCompressResponseWriter) WriteHeader(code int) {
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	meta := compression.ResponseMeta{Method: w.requestMethod, Status: code, Header: w.ResponseWriter.Header().Clone()}
	decision := compression.Decision{Coding: compression.Identity}
	if w.state != nil {
		decision = w.state.Decide(meta)
	}
	if decision.Vary {
		base.AppendVaryToken(w.Header(), "Accept-Encoding")
	}

	// Existing representations are never encoded again.  304 keeps upstream
	// representation metadata and only receives the Vary safety token.
	if headerValue(w.Header(), "Content-Encoding") != "" ||
		code == http.StatusNotModified || code == http.StatusNoContent || code == http.StatusSwitchingProtocols ||
		!base.ResponseAllowsBody(w.requestMethod, code) && !strings.EqualFold(w.requestMethod, http.MethodHead) {
		w.w = w.ResponseWriter
		w.ResponseWriter.WriteHeader(code)
		return
	}

	switch decision.Coding {
	case compression.Gzip:
		w.encoding = encodingGzip
	case compression.Deflate:
		w.encoding = encodingDeflate
	default:
		w.encoding = encodingNone
	}
	if w.encoding != encodingNone {
		base.InvalidateBodyDerivedHeaders(w.Header())
		if strings.EqualFold(w.requestMethod, http.MethodHead) {
			w.ResponseWriter.Header().Set("Content-Encoding", codingHeader(w.encoding))
			w.w = w.ResponseWriter
			w.ResponseWriter.WriteHeader(code)
			return
		}
	}

	if w.encoding == encodingNone {
		w.w = w.ResponseWriter
		w.ResponseWriter.WriteHeader(code)
		return
	}

	compressor, err := acquireCompressionWriter(w.encoding, w.level, w.ResponseWriter)
	if err != nil {
		w.w = w.ResponseWriter
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.compressor = compressor
	w.w = compressor
	w.ResponseWriter.Header().Set("Content-Encoding", codingHeader(w.encoding))
	w.ResponseWriter.WriteHeader(code)
}

func (w *maybeCompressResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if !base.ResponseAllowsBody(w.requestMethod, w.statusCode()) {
		if strings.EqualFold(w.requestMethod, http.MethodHead) {
			return len(p), nil
		}
		return 0, http.ErrBodyNotAllowed
	}
	return w.w.Write(p)
}

func (w *maybeCompressResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if compressor, ok := w.w.(interface{ Flush() error }); ok {
		_ = compressor.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *maybeCompressResponseWriter) Close() error {
	compressor := w.compressor
	w.compressor = nil
	w.w = w.ResponseWriter
	return releaseCompressionWriter(w.encoding, w.level, compressor)
}

func (w *maybeCompressResponseWriter) FinishStreamingResponse(_ error) error {
	if !w.hijacked && !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.Close()
}

func (w *maybeCompressResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func codingHeader(enc encoding) string {
	if enc == encodingDeflate {
		return "deflate"
	}
	return "gzip"
}
