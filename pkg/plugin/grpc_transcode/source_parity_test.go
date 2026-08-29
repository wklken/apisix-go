package grpc_transcode

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// APISIX 3.17 at 9ef2ecab67f652d38365049613610ef649bb4ad0
// registers no protobuf hooks, so enable_hooks has no observable effect.
func TestAPISIX317EnableHooksWithoutRegisteredHooksMatchesDisableHooks(t *testing.T) {
	type observation struct {
		upstreamBody []byte
		status       int
		responseBody string
	}
	run := func(hooksOption string) observation {
		t.Helper()
		restore := stubProtoContent(t, "echo-proto", testDescriptorContent(t))
		defer restore()

		plugin := newTestPlugin(t, Config{
			ProtoID:  "echo-proto",
			Service:  "echo.EchoService",
			Method:   "Echo",
			PBOption: []string{"no_default_values", hooksOption},
		})
		request := httptest.NewRequest(http.MethodGet, "/echo?msg=Hello", nil)
		response := httptest.NewRecorder()
		var upstreamBody []byte
		plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			upstreamBody, err = io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read upstream body: %v", err)
			}
			w.Header().Set("Grpc-Status", "0")
			_, _ = w.Write(frameGRPCMessageForTest(t, encodeEchoMessage(t, "Hello")))
		})).ServeHTTP(response, request)

		return observation{
			upstreamBody: upstreamBody,
			status:       response.Code,
			responseBody: response.Body.String(),
		}
	}

	enabled := run("enable_hooks")
	disabled := run("disable_hooks")
	if !bytes.Equal(enabled.upstreamBody, disabled.upstreamBody) ||
		enabled.status != disabled.status || enabled.responseBody != disabled.responseBody {
		t.Fatalf("enable_hooks observation = %#v, want disable_hooks observation %#v", enabled, disabled)
	}
	if enabled.status != http.StatusOK || enabled.responseBody != `{"msg":"Hello"}` {
		t.Fatalf("enable_hooks response = status %d body %q, want 200 JSON echo", enabled.status, enabled.responseBody)
	}
}
