package base

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	DefaultRequestBodyMaxBytes  = 1 << 20
	DefaultResponseBodyMaxBytes = 4 << 20
)

// BodyTooLargeError reports a bounded read that retained at most Limit+1
// bytes. Callers can map this error to the protocol-specific 413/502 status.
type BodyTooLargeError struct {
	Limit int64
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("body exceeds maximum size %d", e.Limit)
}

func IsBodyTooLarge(err error) bool {
	var sizeErr *BodyTooLargeError
	return errors.As(err, &sizeErr)
}

func ReadRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

// ReadRequestBodyLimited reads and restores the request body while rejecting
// bodies larger than maxSize with a size-exceeded error.
func ReadRequestBodyLimited(r *http.Request, maxSize int) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := readBodyLimited(r.Body, int64(maxSize))
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func ReadResponseBodyLimited(reader io.Reader, maxSize int64) ([]byte, error) {
	return readBodyLimited(reader, maxSize)
}

func readBodyLimited(reader io.Reader, maxSize int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maxSize+1))
	if err == nil && int64(len(body)) > maxSize {
		err = &BodyTooLargeError{Limit: maxSize}
	}
	return body, err
}

func ReplaceRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// ProtocolVersion returns the request protocol version as major.minor.
func ProtocolVersion(r *http.Request) string {
	return strconv.Itoa(r.ProtoMajor) + "." + strconv.Itoa(r.ProtoMinor)
}

// WriteJSONMessage preserves the plugin-base API for existing callers while
// using the canonical response writer in util.
func WriteJSONMessage(w http.ResponseWriter, status int, message string) {
	_ = util.WriteJSONMessage(w, status, message)
}

func RemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func RequestVarFromNginx(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if after, ok := strings.CutPrefix(key, "http_"); ok {
		return r.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}

	value := apisixvar.GetNginxVar(r, "$"+key)
	if key == "remote_addr" {
		return RemoteIP(value)
	}
	return value
}
