package ai_auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
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

	amzDate := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	if opts.IncludePayloadHash {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	if opts.SetSecurityToken && config.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", config.SessionToken)
	}

	canonicalHeaders, signedHeaders := canonicalAWSHeaders(req, opts)
	canonicalRequest := strings.Join([]string{
		req.Method,
		opts.CanonicalURI(req.URL),
		opts.CanonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	if opts.RewriteQuery {
		req.URL.RawQuery = opts.CanonicalQuery(req.URL)
	}
	scope := strings.Join([]string{date, opts.Region, opts.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(
		hmacSHA256(awsSigningKey(config.SecretAccessKey, date, opts.Region, opts.Service), stringToSign),
	)
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		config.AccessKeyID,
		scope,
		signedHeaders,
		signature,
	))
	return nil
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

func canonicalAWSHeaders(req *http.Request, opts SignAWSRequestOptions) (string, string) {
	values := make(map[string]string)
	names := make([]string, 0)
	if opts.DeriveHeadersFromRequest {
		names = append(names, "host")
		values["host"] = opts.HeaderValue(requestHost(req))
		for key, headerValues := range req.Header {
			key = strings.ToLower(key)
			if key == "connection" || key == "host" {
				continue
			}
			normalized := make([]string, 0, len(headerValues))
			for _, value := range headerValues {
				normalized = append(normalized, opts.HeaderValue(value))
			}
			values[key] = strings.Join(normalized, ",")
			names = append(names, key)
		}
	} else {
		if len(opts.CanonicalHeaders) == 0 {
			names = []string{"host", "x-amz-date"}
			if opts.IncludePayloadHash {
				names = append(names, "x-amz-content-sha256")
			}
		} else {
			names = append([]string(nil), opts.CanonicalHeaders...)
		}
		if token := req.Header.Get("X-Amz-Security-Token"); token != "" {
			names = append(names, "x-amz-security-token")
		}
		for _, name := range names {
			if name == "host" {
				values["host"] = opts.HeaderValue(requestHost(req))
			} else {
				values[name] = opts.HeaderValue(req.Header.Get(name))
			}
		}
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteByte(':')
		canonical.WriteString(values[name])
		canonical.WriteByte('\n')
	}
	return canonical.String(), strings.Join(names, ";")
}

func requestHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
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

func awsSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	return mac.Sum(nil)
}
