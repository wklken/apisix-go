package route

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
)

func TestAIExecutionRunsAfterLowerPriorityMiddleware(t *testing.T) {
	events := make([]string, 0, 4)
	selectAI := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			events = append(events, "select")
			r = ai_runtime.WithExecution(r, "model-a", func(w http.ResponseWriter, _ *http.Request) {
				events = append(events, "provider")
				w.WriteHeader(http.StatusCreated)
			})
			next.ServeHTTP(w, r)
		})
	}
	rateLimit := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			events = append(events, "preflight")
			next.ServeHTTP(w, r)
			events = append(events, "charge")
		})
	}
	fallbackCalls := 0
	handler := withAIExecutionTerminal(alice.New(selectAI, rateLimit), http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		fallbackCalls++
	}))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", nil))

	wantEvents := []string{"select", "preflight", "provider", "charge"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("events = %#v, want %#v", events, wantEvents)
	}
	if fallbackCalls != 0 || rr.Code != http.StatusCreated {
		t.Fatalf("fallback calls = %d, response code = %d, want 0 and 201", fallbackCalls, rr.Code)
	}
}

func TestAIExecutionTerminalPreservesOrdinaryRoute(t *testing.T) {
	fallbackCalls := 0
	handler := withAIExecutionTerminal(alice.New(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if fallbackCalls != 1 || rr.Code != http.StatusAccepted {
		t.Fatalf("fallback calls = %d, response code = %d, want 1 and 202", fallbackCalls, rr.Code)
	}
}
