package ai_auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type AWSConfig struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

func SignAWSRequest(req *http.Request, body []byte, config AWSConfig, region, service string, now time.Time) error {
	if config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return fmt.Errorf("AWS access_key_id and secret_access_key are required")
	}
	if region == "" {
		return fmt.Errorf("AWS region is required")
	}
	if service == "" {
		service = "bedrock"
	}

	amzDate := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")
	payloadHash := sha256Hex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if config.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", config.SessionToken)
	}

	canonicalHeaders, signedHeaders := canonicalAWSHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalAWSURI(req.URL),
		canonicalAWSQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(
		hmacSHA256(awsSigningKey(config.SecretAccessKey, date, region, service), stringToSign),
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

func canonicalAWSHeaders(req *http.Request) (string, string) {
	headers := map[string]string{
		"host":                 requestHost(req),
		"x-amz-content-sha256": req.Header.Get("X-Amz-Content-Sha256"),
		"x-amz-date":           req.Header.Get("X-Amz-Date"),
	}
	if token := req.Header.Get("X-Amz-Security-Token"); token != "" {
		headers["x-amz-security-token"] = token
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name)
		canonical.WriteByte(':')
		canonical.WriteString(strings.Join(strings.Fields(headers[name]), " "))
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

func canonicalAWSURI(target *url.URL) string {
	path := target.EscapedPath()
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
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

func canonicalAWSQuery(target *url.URL) string {
	return strings.ReplaceAll(target.Query().Encode(), "+", "%20")
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func awsSigningKey(secret, date, region, service string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+secret), date)
	regionKey := hmacSHA256(dateKey, region)
	serviceKey := hmacSHA256(regionKey, service)
	return hmacSHA256(serviceKey, "aws4_request")
}

func hmacSHA256(key []byte, value string) []byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(value))
	return hash.Sum(nil)
}
