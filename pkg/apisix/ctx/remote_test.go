package ctx

import (
	"context"
	"net/http"
	"testing"
)

func TestEffectiveRemoteIPUsesContextOverrideAndNormalizesIt(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.RemoteAddr = "192.0.2.10:4321"
	request = request.WithContext(context.WithValue(request.Context(), RemoteAddrKey, "[2001:db8::10]:8443"))

	if got := EffectiveRemoteIP(request); got != "2001:db8::10" {
		t.Fatalf("EffectiveRemoteIP() = %q, want 2001:db8::10", got)
	}
	if got := PeerRemoteIP(request); got != "192.0.2.10" {
		t.Fatalf("PeerRemoteIP() = %q, want 192.0.2.10", got)
	}
}

func TestEffectiveRemoteIPFallsBackToSocketPeer(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.RemoteAddr = "[2001:db8::20]:4321"

	if got := EffectiveRemoteIP(request); got != "2001:db8::20" {
		t.Fatalf("EffectiveRemoteIP() = %q, want 2001:db8::20", got)
	}
}
