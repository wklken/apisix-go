package wolf_rbac

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestParityHTTP200SelectsConsumerWithoutOKFlag(t *testing.T) {
	for _, body := range []string{`{"ok":false}`, `{}`, `{"ok":false,"data":{"userInfo":{"username":"alice"}}}`} {
		t.Run(body, func(t *testing.T) {
			wolf := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }),
			)
			defer wolf.Close()
			addWolfConsumer(t, "parity-wolf", "parity-app", wolf.URL)
			p := newTestPlugin(t, Config{})
			request := httptest.NewRequest("GET", "http://example.test/", nil)
			request.Header.Set("Authorization", "V1#parity-app#token")
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if auth, ok := ctx.AuthenticationStateFrom(r); !ok || auth.Consumer().Username != "parity-wolf" {
					t.Error("selected consumer absent")
				}
				w.WriteHeader(204)
			})).ServeHTTP(response, request)
			if response.Code != 204 {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestParityOmittedTLSVerificationAcceptsSelfSignedWolf(t *testing.T) {
	wolf := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }),
	)
	defer wolf.Close()
	p := newTestPlugin(t, Config{})
	cfg := consumerConfig{Server: wolf.URL}
	cfg.applyDefaults(p.config)
	status, _, _, err := p.checkPermission(
		httptest.NewRequest("GET", "/", nil),
		cfg,
		rbacToken{AppID: "app", WolfToken: "token"},
	)
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
}
