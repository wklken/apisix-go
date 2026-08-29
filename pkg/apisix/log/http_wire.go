package log

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"strconv"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const http1ServerBufferBytes = 2048

// EstimateHTTP1RequestLength reconstructs the HTTP/1.1 request length exposed
// by Nginx's $request_length without reading the live request body. Header
// order and casing are immaterial to the byte count after net/http has parsed
// them. Encodings whose framing cannot be recovered return known=false.
func EstimateHTTP1RequestLength(r *http.Request) (size int64, known bool) {
	if r == nil || r.URL == nil || r.ProtoMajor != 1 || r.ProtoMinor != 1 ||
		r.ContentLength < 0 || len(r.TransferEncoding) != 0 {
		return 0, false
	}
	clone := new(http.Request)
	*clone = *r
	clone.Body = nil
	clone.GetBody = nil
	clone.Header = r.Header.Clone()
	if r.ContentLength > 0 {
		clone.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
	}
	if r.Close && clone.Header.Get("Connection") == "" {
		clone.Header.Set("Connection", "close")
	}
	dump, err := httputil.DumpRequest(clone, false)
	if err != nil {
		return 0, false
	}
	return int64(len(dump)) + r.ContentLength, true
}

// EstimateHTTP1ResponseLength reconstructs the bytes emitted by Go's
// net/http HTTP/1.1 server for a completed, buffered response. Flushed,
// hijacked, chunked, trailer-bearing, and other unrecoverable responses fail
// closed instead of reporting a body length as a wire length.
func EstimateHTTP1ResponseLength(
	r *http.Request,
	header http.Header,
	outcome apisixctx.ResponseOutcome,
	bodyPrefix []byte,
) (size int64, known bool) {
	if r == nil || r.ProtoMajor != 1 || r.ProtoMinor != 1 || outcome.Hijacked || outcome.Flushed ||
		outcome.Bytes < 0 || outcome.Bytes > http1ServerBufferBytes ||
		len(header.Values("Transfer-Encoding")) != 0 || len(header.Values("Trailer")) != 0 ||
		r.Method == http.MethodHead || !responseBodyAllowed(outcome.Status) {
		return 0, false
	}

	finalHeader := header.Clone()
	if finalHeader == nil {
		finalHeader = make(http.Header)
	}
	if _, present := finalHeader["Date"]; !present {
		// The value is intentionally fixed: every valid HTTP date has the
		// same wire length, while the live timestamp is not part of the size.
		finalHeader.Set("Date", "Mon, 02 Jan 2006 15:04:05 GMT")
	}
	if _, present := finalHeader["Content-Type"]; !present &&
		finalHeader.Get("Content-Encoding") == "" && outcome.Bytes > 0 {
		if len(bodyPrefix) == 0 {
			return 0, false
		}
		finalHeader.Set("Content-Type", http.DetectContentType(bodyPrefix))
	}
	if finalHeader.Get("Content-Length") == "" {
		finalHeader.Set("Content-Length", strconv.FormatInt(outcome.Bytes, 10))
	}
	if r.Close && finalHeader.Get("Connection") == "" {
		finalHeader.Set("Connection", "close")
	}

	statusText := http.StatusText(outcome.Status)
	if statusText == "" {
		statusText = "status code " + strconv.Itoa(outcome.Status)
	}
	size = int64(len(fmt.Sprintf("HTTP/1.1 %03d %s\r\n", outcome.Status, statusText)))
	for key, values := range finalHeader {
		canonicalKey := http.CanonicalHeaderKey(key)
		for _, value := range values {
			size += int64(len(canonicalKey) + len(value) + len(": \r\n"))
		}
	}
	return size + int64(len("\r\n")) + outcome.Bytes, true
}

func responseBodyAllowed(status int) bool {
	return status >= http.StatusOK && status != http.StatusNoContent && status != http.StatusNotModified
}
