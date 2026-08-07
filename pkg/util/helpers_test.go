package util

import (
	"testing"
)

func TestBytesToStringRoundTrips(t *testing.T) {
	if got := BytesToString(StringToBytes("apisix")); got != "apisix" {
		t.Fatalf("round trip = %q, want apisix", got)
	}
	if got := BytesToString(StringToBytes("")); got != "" {
		t.Fatalf("empty round trip = %q, want empty", got)
	}
}
