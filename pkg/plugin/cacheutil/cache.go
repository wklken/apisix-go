package cacheutil

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// CloneHeader returns a deep copy of header and its value slices.
func CloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for field, values := range header {
		cloned[field] = append([]string(nil), values...)
	}
	return cloned
}

// ParseVaryHeader returns normalized Vary field names and whether the response
// can be cached. A wildcard Vary value makes the response uncacheable.
func ParseVaryHeader(header http.Header) ([]string, bool) {
	values := header.Values("Vary")
	if len(values) == 0 {
		return nil, true
	}

	seen := make(map[string]struct{})
	var headers []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if name == "*" {
				return nil, false
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			headers = append(headers, name)
		}
	}
	sort.Strings(headers)
	return headers, true
}

// VarySignature returns the existing MD5 cache-key suffix for the ordered
// request header values named by headers.
func VarySignature(headers []string, r *http.Request) string {
	var framed strings.Builder
	for _, header := range headers {
		appendFrame(&framed, header)
		values := r.Header.Values(header)
		framed.WriteString(strconv.Itoa(len(values)))
		framed.WriteByte(':')
		for _, value := range values {
			appendFrame(&framed, value)
		}
	}
	sum := md5.Sum([]byte(framed.String()))
	return hex.EncodeToString(sum[:])
}

func appendFrame(builder *strings.Builder, value string) {
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
}
