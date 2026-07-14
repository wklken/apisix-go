package ctx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestAttachConsumerSetsUpstreamUsernameHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = WithApisixVars(req, map[string]string{})

	AttachConsumer(req, resource.Consumer{Username: "bob"})

	if got := req.Header.Get("X-Consumer-Username"); got != "bob" {
		t.Fatalf("X-Consumer-Username = %q, want bob", got)
	}
}
