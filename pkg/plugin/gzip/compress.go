package gzip

// we need to use the maybeCompressResponseWriter, not the middleware, so we need to copy the code here(it's not exported)

// This file is a copy of
// reference: https://github.com/Go-chi/chi/blob/v1.0.0/middleware/compress.go
// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Under the MIT License

import (
	"bufio"
	"compress/flate"
	cgzip "compress/gzip"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

func selectEncoding(h http.Header) encoding {
	enc := h.Get("Accept-Encoding")
	if enc == "" {
		return encodingNone
	}

	gzipQuality := -1.0
	deflateQuality := -1.0
	wildcardQuality := -1.0
	for part := range strings.SplitSeq(enc, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		coding, quality := part, 1.0
		if before, after, found := strings.Cut(part, ";"); found {
			coding = strings.TrimSpace(before)
			if params := strings.TrimSpace(after); strings.HasPrefix(strings.ToLower(params), "q=") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(params[2:]), 64); err == nil {
					quality = max(0, min(parsed, 1))
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip":
			if quality > gzipQuality {
				gzipQuality = quality
			}
		case "deflate":
			if quality > deflateQuality {
				deflateQuality = quality
			}
		case "*":
			if quality > wildcardQuality {
				wildcardQuality = quality
			}
		}
	}

	switch {
	case gzipQuality >= 0 || deflateQuality >= 0:
		// Explicit codings decide before the wildcard: a zero quality
		// disables that coding, and equal qualities prefer gzip.
		if gzipQuality > deflateQuality {
			if gzipQuality == 0 {
				return encodingNone
			}
			return encodingGzip
		}
		if deflateQuality > gzipQuality {
			if deflateQuality == 0 {
				return encodingNone
			}
			return encodingDeflate
		}
		if gzipQuality == 0 {
			return encodingNone
		}
		return encodingGzip
	case wildcardQuality >= 0:
		if wildcardQuality == 0 {
			return encodingNone
		}
		return encodingGzip
	default:
		return encodingNone
	}
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
		return flate.NewWriter(destination, level)
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
	return flate.NewWriter(destination, level)
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
	w            io.Writer
	compressor   resettableWriteCloser
	encoding     encoding
	contentTypes map[string]struct{}
	level        int
	wroteHeader  bool
	wildcardType bool
	minLength    int
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
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *maybeCompressResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	defer w.ResponseWriter.WriteHeader(code)

	// Already compressed data?
	if w.ResponseWriter.Header().Get("Content-Encoding") != "" {
		return
	}

	// Parse the first part of the Content-Type response header.
	contentType := ""
	parts := strings.Split(w.ResponseWriter.Header().Get("Content-Type"), ";")
	if len(parts) > 0 {
		contentType = parts[0]
	}

	// Is the content type compressable?
	if !w.wildcardType {
		if _, ok := w.contentTypes[contentType]; !ok {
			return
		}
	}

	contentLength := w.ResponseWriter.Header().Get("Content-Length")
	if contentLength != "" {
		length, err := strconv.Atoi(contentLength)
		if err == nil && length < w.minLength {
			return
		}
	}

	if w.encoding != encodingNone {
		w.ResponseWriter.Header().Del("Content-Length")
	}

	if w.encoding == encodingNone {
		return
	}

	compressor, err := acquireCompressionWriter(w.encoding, w.level, w.ResponseWriter)
	if err != nil {
		w.w = w.ResponseWriter
		return
	}
	w.compressor = compressor
	w.w = compressor
	if w.encoding == encodingGzip {
		w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	} else {
		w.ResponseWriter.Header().Set("Content-Encoding", "deflate")
	}
}

func (w *maybeCompressResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
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
