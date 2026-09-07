package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpstreamTimeoutUsesAPISIXDefaultHTML(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	newErrorHandler(&testEffectiveConfig().Config)(response, request, routeNetError{timeout: true})
	const wantBody = "<html>\r\n<head><title>504 Gateway Time-out</title></head>\r\n<body>\r\n<center><h1>504 Gateway Time-out</h1></center>\r\n<hr><center>openresty</center>\r\n<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n</html>\r\n"
	if response.Code != http.StatusGatewayTimeout ||
		response.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		response.Body.String() != wantBody {
		t.Fatalf(
			"response = %d %q %q; want official 504 HTML",
			response.Code,
			response.Header().Get("Content-Type"),
			response.Body.String(),
		)
	}
}
