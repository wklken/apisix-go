package ai_common

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"syscall"
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

// ProviderRequestErrorStatus maps LLM transport failures the same way as
// APISIX 3.17 ai-proxy: 504 on timeout, otherwise 500.
func ProviderRequestErrorStatus(err error) int {
	if isTimeoutError(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var timeoutErr net.Error
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	return false
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
