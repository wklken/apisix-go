package limit_conn

import (
	"net/http/httptest"
	"testing"
)

func TestParityCompoundDefaultKey(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			Conn:             1,
			Burst:            0,
			DefaultConnDelay: 0.1,
			KeyType:          "var_combination",
			Key:              "tenant:${http_x_tenant ?? anonymous}",
		},
	)
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	if got := p.resolveKey(r); got != "tenant:anonymous" {
		t.Fatalf("key=%q, want tenant:anonymous", got)
	}
}
