package base

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestPlan16StreamingPhaseContracts(t *testing.T) {
	if ResponseModeBounded != 1 || ResponseModeStreaming != 2 || ResponseModeHijack != 4 {
		t.Fatalf("response mode mask values = %d/%d/%d", ResponseModeBounded, ResponseModeStreaming, ResponseModeHijack)
	}
	if ProtocolResponded != 1 || ProtocolHijacked != 2 {
		t.Fatalf("protocol disposition values = %d/%d", ProtocolResponded, ProtocolHijacked)
	}
	state := StreamingResponseState{
		Status:  http.StatusAccepted,
		Header:  http.Header{"X-Test": {"yes"}},
		Trailer: http.Header{"X-Trailer": {"value"}},
	}
	if state.Status != http.StatusAccepted || state.Header.Get("X-Test") != "yes" ||
		state.Trailer.Get("X-Trailer") != "value" {
		t.Fatalf("streaming response state = %#v", state)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	got := StopRequestWithSource(request, apisixctx.ResponseSourceAPISIX).Source
	if got != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("stop source = %q, want apisix", got)
	}
}
