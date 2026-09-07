package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/expr"
)

func TestRequestIDResponseHeaderAndVariables(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		for _, existing := range []string{"", "upstream-id"} {
			for _, include := range []bool{false, true} {
				config := `{"include_in_response":true}`
				if !include {
					config = `{"include_in_response":false}`
				}
				binding := parityBinding(t, "request-id", config, ScopeRoute)
				bindings := []Binding{binding}
				if buffered {
					bindings = append(bindings, parityBinding(t, "error-page", `{}`, ScopeRoute))
				}
				plan, err := BuildResponsePlan(bindings)
				if err != nil {
					t.Fatal(err)
				}
				handler := plan.Install(
					NewRequestPipeline(bindings, nil),
					http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						native, _ := expr.RequestValue(r, "request_id").(string)
						if len(native) != 32 || strings.Trim(native, "0123456789abcdef") != "" {
							t.Errorf("native request_id = %q; want 32 hex", native)
						}
						if got := expr.RequestValue(r, "apisix_request_id"); got != "client-id" {
							t.Errorf("apisix_request_id = %v; want client-id", got)
						}
						if w.Header().Get("X-Request-Id") != "" {
							t.Error("request-id response header written before header filter")
						}
						if existing != "" {
							w.Header().Add("X-Request-Id", existing)
						}
						apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte("ok"))
					}),
				)
				request, _ := apisixctx.EnsureRequestLifecycle(
					httptest.NewRequest(http.MethodGet, "/", nil),
					time.Now(),
				)
				request.Header.Set("X-Request-Id", "client-id")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want := existing
				if want == "" && include {
					want = "client-id"
				}
				values := response.Header().Values("X-Request-Id")
				if response.Code != 200 || response.Header().Get("X-Request-Id") != want || len(values) > 1 {
					t.Fatalf(
						"buffered=%t include=%t existing=%q response=%d header=%v; want %q",
						buffered,
						include,
						existing,
						response.Code,
						values,
						want,
					)
				}
			}
		}
	}
}

func TestRequestIDHeaderSurvivesMockingEarlyStop(t *testing.T) {
	bindings := []Binding{
		parityBinding(t, "request-id", `{"include_in_response":true}`, ScopeRoute),
		parityBinding(t, "mocking", `{"response_status":200,"response_example":"ok"}`, ScopeRoute),
	}
	plan, err := BuildResponsePlan(bindings)
	if err != nil {
		t.Fatal(err)
	}
	handler := plan.Install(
		NewRequestPipeline(bindings, nil),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected upstream") }),
	)
	request, _ := apisixctx.EnsureRequestLifecycle(httptest.NewRequest(http.MethodGet, "/", nil), time.Now())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || response.Header().Get("X-Request-Id") == "" {
		t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
	}
}

func TestEarlyAndTerminalResponseHeadersRunOnce(t *testing.T) {
	for _, stage := range []string{"rewrite", "access", "terminal"} {
		for _, buffered := range []bool{false, true} {
			calls := 0
			binding := parityBinding(t, "request-id", `{}`, ScopeRoute)
			binding.Plugin = &countingRequestIDHeaderPlugin{Plugin: binding.Plugin, calls: &calls}
			bindings := []Binding{binding}
			switch stage {
			case "rewrite":
				bindings = append(
					bindings,
					parityBinding(t, "mocking", `{"response_status":200,"response_example":"ok"}`, ScopeRoute),
				)
			case "access":
				bindings = append(
					bindings,
					parityBinding(t, "ai-prompt-guard", `{"deny_patterns":["blocked"]}`, ScopeRoute),
				)
			}
			if buffered {
				bindings = append(bindings, parityBinding(t, "error-page", `{}`, ScopeRoute))
			}
			plan, err := BuildResponsePlan(bindings)
			if err != nil {
				t.Fatal(err)
			}
			handler := plan.Install(
				NewRequestPipeline(
					bindings,
					func(r *http.Request) (ConsumerResolution, error) { return ConsumerResolution{Request: r}, nil },
				),
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stage != "terminal" {
						t.Fatal("early stop reached terminal")
					}
					apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
					w.WriteHeader(200)
					_, _ = w.Write([]byte("ok"))
				}),
			)
			request, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(
					http.MethodPost,
					"/",
					strings.NewReader(`{"messages":[{"role":"user","content":"blocked"}]}`),
				),
				time.Now(),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if calls != 1 || len(response.Header().Values("X-Filter")) != 1 {
				t.Fatalf("stage=%s buffered=%t calls=%d headers=%v", stage, buffered, calls, response.Header())
			}
		}
	}
}

type countingRequestIDHeaderPlugin struct {
	Plugin
	calls *int
}

func (p *countingRequestIDHeaderPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	return p.Plugin.(base.RequestPhasePlugin).RunRequestPhase(w, r)
}

func (p *countingRequestIDHeaderPlugin) RunStreamingHeaderFilter(
	r *http.Request,
	state *base.StreamingResponseState,
) error {
	*p.calls++
	state.Header.Add("X-Filter", "once")
	return p.Plugin.(base.StreamingHeaderFilterPlugin).RunStreamingHeaderFilter(r, state)
}

func TestConsumerRequestIDHeaderSurvivesAccessStop(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		bindings := []Binding{parityBinding(t, "ai-prompt-guard", `{"deny_patterns":["blocked"]}`, ScopeRoute)}
		if buffered {
			bindings = append(bindings, parityBinding(t, "error-page", `{}`, ScopeRoute))
		}
		consumer := parityBinding(t, "request-id", `{}`, ScopeConsumer)
		calls := 0
		consumer.Plugin = &countingRequestIDHeaderPlugin{Plugin: consumer.Plugin, calls: &calls}
		resolve := func(r *http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{consumer}}, nil
		}
		plan, err := BuildResponsePlan(bindings)
		if err != nil {
			t.Fatal(err)
		}
		handler := plan.Install(
			NewRequestPipeline(bindings, resolve),
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("guard stop reached upstream") }),
		)
		request, _ := apisixctx.EnsureRequestLifecycle(
			httptest.NewRequest(
				http.MethodPost,
				"/",
				strings.NewReader(`{"messages":[{"role":"user","content":"blocked"}]}`),
			),
			time.Now(),
		)
		request.Header.Set("X-Request-Id", "consumer-id")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 400 || response.Header().Get("X-Request-Id") != "consumer-id" || calls != 1 {
			t.Fatalf("buffered=%t status=%d header=%v calls=%d", buffered, response.Code, response.Header(), calls)
		}
	}
}
