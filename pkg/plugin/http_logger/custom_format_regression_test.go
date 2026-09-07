package http_logger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// Review overlay: APISIX 3.17 get_log_entry uses custom log_format XOR get_full_log.
// include_req_body / include_resp_body are applied only in get_full_log.
func TestCustomLogFormatDoesNotInjectIncludeBodies(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:              server.URL,
		BatchMaxSize:     1,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		LogFormat:        map[string]any{"case": "request"},
	})
	if err := p.RunLogPhase(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodPost, URI: "/body-request", Body: []byte("request-body"),
		},
		Response: apisixlog.ResponseLogSnapshot{Body: []byte("response-body")},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusOK},
	}); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}

	select {
	case body := <-received:
		raw, _ := json.Marshal(body)
		t.Logf("payload=%s", raw)
		if body["case"] != "request" {
			t.Fatalf("case = %#v", body["case"])
		}
		req, _ := body["request"].(map[string]any)
		resp, _ := body["response"].(map[string]any)
		if req["body"] != nil {
			t.Fatalf("request.body = %#v, APISIX 3.17 custom format omits this nested body", req["body"])
		}
		if resp["body"] != nil {
			t.Fatalf("response.body = %#v, APISIX 3.17 custom format omits this nested body", resp["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http-logger payload")
	}
}

func TestEmptyLogFormatObjectUsesCustomFormat(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		BatchMaxSize: 1,
		LogFormat:    map[string]any{},
	})
	if err := p.RunLogPhase(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Method: http.MethodGet, URI: "/empty-format"},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK},
	}); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case body := <-received:
		raw, _ := json.Marshal(body)
		t.Logf("empty-format payload=%s", raw)
		if len(body) != 0 {
			t.Fatal("empty log_format should produce an empty custom entry")
		}
		if body["server"] != nil {
			t.Fatal("unexpected server block on empty log_format")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for empty-format payload")
	}
}
