package ai_common

import (
	"crypto/tls"
	"net/http"
	"time"
)

// ApplyTransportKeepalive applies the shared keepalive pool, timeout, and
// disable-keepalive options to an already cloned transport.
func ApplyTransportKeepalive(transport *http.Transport, pool int, timeoutMS int, keepalive *bool) {
	transport.MaxIdleConnsPerHost = pool
	transport.IdleConnTimeout = time.Duration(timeoutMS) * time.Millisecond
	if keepalive != nil && !*keepalive {
		transport.DisableKeepAlives = true
	}
}

// ApplyTransportSSLVerify disables TLS certificate verification when verify is
// non-nil and false.
func ApplyTransportSSLVerify(transport *http.Transport, verify *bool) {
	if verify != nil && !*verify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
}

// HasProtocolRequestBodyOverride reports whether values contains a key for a
// protocol-specific request body override.
func HasProtocolRequestBodyOverride(values map[string]any) bool {
	for key := range values {
		switch key {
		case "openai-chat", "openai-responses", "openai-embeddings", "anthropic-messages",
			"bedrock-converse", "passthrough":
			return true
		}
	}
	return false
}
