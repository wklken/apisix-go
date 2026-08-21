package ctx

import (
	"net"
	"net/http"
	"strings"
)

// EffectiveRemoteIP returns the client address selected by the real-ip phase,
// falling back to the socket peer when no trusted override is present.
func EffectiveRemoteIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := GetString(r.Context(), string(RemoteAddrKey)); strings.TrimSpace(value) != "" {
		return normalizeRemoteIP(value)
	}
	return PeerRemoteIP(r)
}

// PeerRemoteIP returns only the address observed on the request socket. It
// deliberately ignores any real-ip context override and forwarding headers.
func PeerRemoteIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	return normalizeRemoteIP(r.RemoteAddr)
}

func normalizeRemoteIP(value string) string {
	value = strings.TrimSpace(value)
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(value, "[]")
}
