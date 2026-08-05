package gzip

// we need to use the maybeCompressResponseWriter, not the middleware, so we need to copy the code here(it's not exported)

// This file is a copy of
// reference: https://github.com/Go-chi/chi/blob/v1.0.0/middleware/compress.go
// Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.
// Under the MIT License

import (
	"compress/flate"
	cgzip "compress/gzip"
	"io"
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

	switch {
	// TODO:
	// case "br":    // Brotli, experimental. Firefox 2016, to-be-in Chromium.
	// case "lzma":  // Opera.
	// case "sdch":  // Chrome, Android. Gzip output + dictionary header.

	case strings.Contains(enc, "gzip"):
		// TODO: Exception for old MSIE browsers that can't handle non-HTML?
		// https://zoompf.com/blog/2012/02/lose-the-wait-http-compression
		return encodingGzip

	case strings.Contains(enc, "deflate"):
		// HTTP 1.1 "deflate" (RFC 2616) stands for DEFLATE data (RFC 1951)
		// wrapped with zlib (RFC 1950). The zlib wrapper uses Adler-32
		// checksum compared to CRC-32 used in "gzip" and thus is faster.
		//
		// But.. some old browsers (MSIE, Safari 5.1) incorrectly expect
		// raw DEFLATE data only, without the mentioned zlib wrapper.
		// Because of this major confusion, most modern browsers try it
		// both ways, first looking for zlib headers.
		// Quote by Mark Adler: http://stackoverflow.com/a/9186091/385548
		//
		// The list of browsers having problems is quite big, see:
		// http://zoompf.com/blog/2012/02/lose-the-wait-http-compression
		// https://web.archive.org/web/20120321182910/http://www.vervestudios.co/projects/compression-tests/results
		//
		// That's why we prefer gzip over deflate. It's just more reliable
		// and not significantly slower than gzip.
		return encodingDeflate

		// NOTE: Not implemented, intentionally:
		// case "compress": // LZW. Deprecated.
		// case "bzip2":    // Too slow on-the-fly.
		// case "zopfli":   // Too slow on-the-fly.
		// case "xz":       // Too slow on-the-fly.
	}

	return encodingNone
}

type resettableWriteCloser interface {
	io.WriteCloser
	Reset(io.Writer)
}

var gzipWriterPools [10]sync.Pool
var deflateWriterPools [10]sync.Pool

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
