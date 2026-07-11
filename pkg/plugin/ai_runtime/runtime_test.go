package ai_runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerminalHandlerExecutesAIRequestExactlyOnce(t *testing.T) {
	executions := 0
	fallbackCalls := 0
	req := WithExecution(httptest.NewRequest(http.MethodPost, "/", nil), "model-a", func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		executions++
		w.WriteHeader(http.StatusCreated)
	})
	rr := httptest.NewRecorder()

	TerminalHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		fallbackCalls++
	})).ServeHTTP(rr, req)

	if executions != 1 || fallbackCalls != 0 {
		t.Fatalf("executions = %d, fallback calls = %d, want 1 and 0", executions, fallbackCalls)
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want 201", rr.Code)
	}
}

func TestTerminalHandlerDelegatesOrdinaryRequest(t *testing.T) {
	calls := 0
	rr := httptest.NewRecorder()
	TerminalHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if calls != 1 || rr.Code != http.StatusAccepted {
		t.Fatalf("fallback calls = %d, response code = %d, want 1 and 202", calls, rr.Code)
	}
}

func TestSelectedInstanceCanChangeDuringExecution(t *testing.T) {
	req := WithExecution(httptest.NewRequest(http.MethodPost, "/", nil), "first", func(
		_ http.ResponseWriter,
		r *http.Request,
	) {
		FromRequest(r).SetInstanceName("second")
	})
	TerminalHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(),
		req,
	)

	if got, ok := SelectedInstanceName(req); !ok || got != "second" {
		t.Fatalf("selected instance = %q, %v, want second, true", got, ok)
	}
}

func TestEnableTerminalMarksRequest(t *testing.T) {
	EnableTerminal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !TerminalEnabled(r) {
			t.Fatal("terminal execution marker is not enabled")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestStreamingStateCanBePublishedBeforeExecution(t *testing.T) {
	req := WithExecution(httptest.NewRequest(http.MethodPost, "/", nil), "model-a", nil)
	state := FromRequest(req)
	state.SetStreaming(true)

	if !state.Streaming() {
		t.Fatal("streaming state = false, want true")
	}
}

func TestRateLimitFallbackAdvancesSelectedInstance(t *testing.T) {
	req := WithSelectedInstanceName(httptest.NewRequest(http.MethodPost, "/", nil), "first")
	state := FromRequest(req)
	state.ConfigureRateLimitFallback(true, func() bool {
		state.SetInstanceName("second")
		return true
	})

	if !state.RateLimitFallbackEnabled() || !state.AdvanceRateLimitTarget() {
		t.Fatal("rate-limit fallback did not advance")
	}
	if got, ok := SelectedInstanceName(req); !ok || got != "second" {
		t.Fatalf("selected instance = %q, %v, want second, true", got, ok)
	}
}
