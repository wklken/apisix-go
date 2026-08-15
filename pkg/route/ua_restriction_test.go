package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUARestrictionRejectsInvalidRegexAndKeepsLastGoodHandler(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t, "ua-restriction")
	const routeID = "ua-restriction-regex"
	putRouteResource(t, routeID, []byte(`{
		"id":"ua-restriction-regex",
		"uri":"/ua-regex",
		"plugins":{"ua-restriction":{"denylist":["blocked"]}}
	}`))

	validBuilder := NewBuilder(nil)
	t.Cleanup(validBuilder.Stop)
	lastGood, err := validBuilder.BuildStrict()
	if err != nil {
		t.Fatalf("valid BuildStrict() error = %v", err)
	}

	putRouteResource(t, routeID, []byte(`{
		"id":"ua-restriction-regex",
		"uri":"/ua-regex",
		"plugins":{"ua-restriction":{"denylist":["[invalid"]}}
	}`))
	invalidBuilder := NewBuilder(nil)
	t.Cleanup(invalidBuilder.Stop)
	handler, err := invalidBuilder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("invalid BuildStrict() = (%T, %v), want nil handler and error", handler, err)
	}
	for _, want := range []string{routeID, "denylist[0]", "[invalid"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("BuildStrict() error = %q, want %q", err, want)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/ua-regex", nil)
	request.Header.Del("User-Agent")
	response := httptest.NewRecorder()
	lastGood.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("last-good handler status = %d, want 403", response.Code)
	}
}
