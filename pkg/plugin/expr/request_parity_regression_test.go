package expr

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRequestAndSnapshotVariablesPreserveRawArgumentsAndHeaderSeparators(t *testing.T) {
	request := httptest.NewRequest("GET", "/vars?k&k=a%20b+c;tail=1&k=later", nil)
	request.Header = http.Header{"X-Role": {"one", "two"}, "Cookie": {"a=1", "b=2"}}
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			URI:    request.URL.RequestURI(),
			Header: request.Header,
			Query:  request.URL.Query(),
		},
	}
	for _, test := range []struct{ name, want string }{
		{"arg_k", "a%20b+c;tail=1"},
		{"http_x_role", "one, two"},
		{"http_cookie", "a=1; b=2"},
	} {
		if got := RequestValue(request, test.name); got != test.want {
			t.Fatalf("live %s=%#v want=%q", test.name, got, test.want)
		}
		if got := SnapshotValue(snapshot, test.name); got != test.want {
			t.Fatalf("snapshot %s=%#v want=%q", test.name, got, test.want)
		}
	}
}
