package server_info

import "testing"

func TestServerInfoVersion(t *testing.T) {
	got := CurrentInfo("node-a").Version
	if got != "3.17.0" {
		t.Fatalf("version = %q, want official APISIX 3.17.0", got)
	}
}
