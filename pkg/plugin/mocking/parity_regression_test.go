package mocking

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
	"github.com/wklken/apisix-go/pkg/version"
)

func TestParityOfficialAcceptsInformationalMockStatus(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := util.Validate(map[string]any{
		"response_example": "ok",
		"response_status":  199,
	}, p.GetSchema())
	if err != nil {
		t.Fatalf("response_status=199 rejected; APISIX 3.17 schema minimum is 100: %v", err)
	}
}

func TestParityOfficialMockHeaderValue(t *testing.T) {
	example := "ok"
	p := newTestPlugin(t, Config{ResponseExample: &example})
	recorder := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/", nil),
	)
	if got := recorder.Header().Get("x-mock-by"); got != "APISIX/"+version.Version {
		t.Fatalf("x-mock-by = %q, want APISIX/%s", got, version.Version)
	}
}
