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
	"strings"
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

type maybeCompressResponseWriter struct {
	http.ResponseWriter
	w            io.Writer
	encoding     encoding
	contentTypes map[string]struct{}
	level        int
	wroteHeader  bool
}

func (w *maybeCompressResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
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
	if _, ok := w.contentTypes[contentType]; !ok {
		return
	}

	// Select the compress writer.
	switch w.encoding {
	case encodingGzip:
		gw, err := cgzip.NewWriterLevel(w.ResponseWriter, w.level)
		if err != nil {
			w.w = w.ResponseWriter
			return
		}
		w.w = gw
		w.ResponseWriter.Header().Set("Content-Encoding", "gzip")

	case encodingDeflate:
		dw, err := flate.NewWriter(w.ResponseWriter, w.level)
		if err != nil {
			w.w = w.ResponseWriter
			return
		}
		w.w = dw
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
	if f, ok := w.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *maybeCompressResponseWriter) Close() error {
	if c, ok := w.w.(io.WriteCloser); ok {
		return c.Close()
	}
	return nil
}
