package ai_auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type AWSConfig struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

// SignAWSRequestOptions controls the AWS SigV4 signing behavior. Zero values
// preserve the default signer behavior.
type SignAWSRequestOptions struct {
	Region  string
	Service string

	// IncludePayloadHash sets the X-Amz-Content-Sha256 header.
	IncludePayloadHash bool
	// SetSecurityToken sets the X-Amz-Security-Token header from
	// config.SessionToken.
	SetSecurityToken bool
	// RewriteQuery writes the canonical query back into r.URL.RawQuery.
	RewriteQuery bool
	// DeriveHeadersFromRequest signs every request header (excluding
	// connection/host) instead of CanonicalHeaders.
	DeriveHeadersFromRequest bool
	// CanonicalHeaders lists the exact lowercase header names to sign.
	// When empty and DeriveHeadersFromRequest is false, the default set is
	// host, x-amz-date, plus x-amz-content-sha256 when IncludePayloadHash,
	// plus x-amz-security-token when the token header is set.
	CanonicalHeaders []string
	// HeaderValue normalizes a single header value; defaults to
	// whitespace-collapse.
	HeaderValue func(string) string
	// CanonicalURI canonicalizes the request path; defaults to
	// double-escaped path segments.
	CanonicalURI func(u *url.URL) string
	// CanonicalQuery canonicalizes the query string; defaults to the sorted
	// url.Values encoding with %20 for spaces.
	CanonicalQuery func(u *url.URL) string
}

// SignAWSRequest signs a request with the default AWS SigV4 behavior.
func SignAWSRequest(req *http.Request, body []byte, config AWSConfig, region, service string, now time.Time) error {
	return SignAWSRequestWithOptions(req, body, config, SignAWSRequestOptions{
		Region:             region,
		Service:            service,
		IncludePayloadHash: true,
		SetSecurityToken:   true,
	}, now)
}

// SignAWSRequestWithOptions signs a request with the given AWS SigV4 behavior.
func SignAWSRequestWithOptions(
	req *http.Request,
	body []byte,
	config AWSConfig,
	opts SignAWSRequestOptions,
	now time.Time,
) error {
	if config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return fmt.Errorf("AWS access_key_id and secret_access_key are required")
	}
	if opts.Region == "" {
		return fmt.Errorf("AWS region is required")
	}
	if opts.Service == "" {
		opts.Service = "bedrock"
	}
	if opts.HeaderValue == nil {
		opts.HeaderValue = normalizeHeaderValue
	}
	if opts.CanonicalURI == nil {
		opts.CanonicalURI = canonicalURISegments
	}
	if opts.CanonicalQuery == nil {
		opts.CanonicalQuery = canonicalQueryEncoded
	}

	credentials := aws.Credentials{
		AccessKeyID:     config.AccessKeyID,
		SecretAccessKey: config.SecretAccessKey,
		SessionToken:    config.SessionToken,
	}
	if !opts.SetSecurityToken {
		credentials.SessionToken = ""
	}

	amzDate := now.UTC().Format("20060102T150405Z")
	// The SDK signs every header present on the request, so reduce the
	// request to exactly the header set this mode signs, then restore the
	// stripped headers afterwards.
	stripped := scopeSignedHeaders(req, opts)
	req.Header.Set("X-Amz-Date", amzDate)
	if opts.IncludePayloadHash {
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	}
	originalQuery := req.URL.RawQuery
	originalLength := req.ContentLength
	req.ContentLength = 0

	err := signWithSDK(req.Context(), req, sha256Hex(body), credentials, opts, now)

	for name, values := range stripped {
		if isSigningHeader(name) {
			continue
		}
		req.Header[name] = values
	}
	req.ContentLength = originalLength
	if !opts.RewriteQuery {
		req.URL.RawQuery = originalQuery
	}
	return err
}

func signWithSDK(
	ctx context.Context,
	req *http.Request,
	payloadHash string,
	credentials aws.Credentials,
	options SignAWSRequestOptions,
	now time.Time,
) error {
	return v4.NewSigner().SignHTTP(
		ctx, credentials, req, payloadHash,
		options.Service, options.Region, now,
		func(o *v4.SignerOptions) {
			o.DisableURIPathEscaping = reflect.ValueOf(options.CanonicalURI).Pointer() ==
				reflect.ValueOf(CanonicalURIPlain).Pointer()
		},
	)
}

// scopeSignedHeaders removes request headers that must not be signed for the
// selected mode and returns them so the caller can restore them. The SDK
// always adds host, x-amz-date and optionally x-amz-security-token itself.
func scopeSignedHeaders(req *http.Request, opts SignAWSRequestOptions) map[string][]string {
	stripped := req.Header.Clone()
	if opts.DeriveHeadersFromRequest {
		// host is provided by the signer; connection and content-length are
		// never signed.
		req.Header.Del("Connection")
		req.Header.Del("Content-Length")
		req.Header.Del("Host")
		return stripped
	}
	if len(opts.CanonicalHeaders) > 0 {
		keep := make(http.Header)
		for _, name := range opts.CanonicalHeaders {
			// host and the signing headers are provided by the signer.
			if name == "host" || isSigningHeader(name) {
				continue
			}
			if values, ok := stripped[textproto.CanonicalMIMEHeaderKey(name)]; ok {
				keep[name] = values
			}
		}
		req.Header = keep
		return stripped
	}
	// Default mode signs only host, x-amz-date, x-amz-content-sha256 and
	// x-amz-security-token.
	req.Header = make(http.Header)
	return stripped
}

func isSigningHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-amz-date", "x-amz-security-token", "x-amz-content-sha256":
		return true
	default:
		return false
	}
}

// CanonicalURIPlain canonicalizes the request path as the escaped path
// verbatim.
func CanonicalURIPlain(target *url.URL) string {
	if target.EscapedPath() == "" {
		return "/"
	}
	return target.EscapedPath()
}

// CanonicalURICleaned canonicalizes the request path with path.Clean.
func CanonicalURICleaned(target *url.URL) string {
	if target.Path == "" {
		return "/"
	}
	cleaned := path.Clean(target.Path)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

// CanonicalQueryRaw canonicalizes the query string via url.Values.Encode
// without percent-escaping spaces.
func CanonicalQueryRaw(target *url.URL) string {
	if target.RawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return target.RawQuery
	}
	return values.Encode()
}

// CanonicalQuerySortedParts canonicalizes the query string by sorting
// individual key=value parts and percent-escaping spaces.
func CanonicalQuerySortedParts(target *url.URL) string {
	rawQuery := target.RawQuery
	if rawQuery == "" {
		return ""
	}
	type queryPart struct {
		key   string
		value string
	}
	parts := make([]queryPart, 0, strings.Count(rawQuery, "&")+1)
	for part := range strings.SplitSeq(rawQuery, "&") {
		key, value, found := strings.Cut(part, "=")
		key = escapeQueryPart(unescapeQueryPart(key))
		if found {
			value = escapeQueryPart(unescapeQueryPart(value))
		}
		parts = append(parts, queryPart{key: key, value: value})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].key != parts[j].key {
			return parts[i].key < parts[j].key
		}
		return parts[i].value < parts[j].value
	})
	var query strings.Builder
	for i, part := range parts {
		if i > 0 {
			query.WriteByte('&')
		}
		query.WriteString(part.key)
		query.WriteByte('=')
		query.WriteString(part.value)
	}
	return query.String()
}

// canonicalURISegments canonicalizes the path as double-escaped segments.
func canonicalURISegments(target *url.URL) string {
	value := target.EscapedPath()
	if value == "" {
		return "/"
	}
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			decoded = segment
		}
		once := url.PathEscape(decoded)
		segments[i] = url.PathEscape(once)
	}
	return strings.Join(segments, "/")
}

// canonicalQueryEncoded canonicalizes the query string via url.Values.Encode
// with spaces percent-escaped.
func canonicalQueryEncoded(target *url.URL) string {
	return strings.ReplaceAll(target.Query().Encode(), "+", "%20")
}

func normalizeHeaderValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func escapeQueryPart(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

func unescapeQueryPart(value string) string {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
