package public_api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMissingPublicAPIReturnsDefaultHTMLPage(t *testing.T) {
	const want = "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>openresty</center>\r\n<p><em>Powered by <a href=\"https://apisix.apache.org/\">APISIX</a>.</em></p></body>\r\n</html>\r\n"
	p := &Plugin{registry: NewRegistry()}
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if response.Code != 404 || response.Body.String() != want ||
		response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}
