package plugin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestPromptGuardStopSurvivesResponseRewrite(t *testing.T) {
	bindings := []Binding{
		parityBinding(t, "ai-prompt-guard", `{"deny_patterns":["blocked"]}`, ScopeRoute),
		parityBinding(t, "response-rewrite", `{"body":"rewritten","headers":{"set":{"X-Rewritten":"1"}}}`, ScopeRoute),
	}
	plan, err := BuildResponsePlan(bindings)
	if err != nil {
		t.Fatal(err)
	}
	handler := plan.Install(
		NewRequestPipeline(bindings, nil),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("denied request reached upstream") }),
	)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(
			http.MethodPost,
			"/",
			strings.NewReader(`{"messages":[{"role":"user","content":"blocked"}]}`),
		),
		time.Now(),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || response.Body.String() != "rewritten" || response.Header().Get("X-Rewritten") != "1" {
		t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("source=%s", lifecycle.ResponseSource())
	}
}

func TestAIRequestRewriteRunsAfterDecoratorAndAWSRewrite(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%t", blocked), func(t *testing.T) {
			var providerBody string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				providerBody = string(body)
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"rewritten-from-llm"}}]}`)
			}))
			defer provider.Close()
			moderation := newExecutorRequestPlugin(
				"ai-aws-content-moderation",
				1050,
				func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
					if blocked {
						w.WriteHeader(400)
						return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
					}
					return base.ContinueRequest(r)
				},
			)
			bindings := []Binding{
				parityBinding(
					t,
					"ai-prompt-decorator",
					`{"prepend":[{"role":"system","content":"decorator-first"}]}`,
					ScopeRoute,
				),
				parityBinding(
					t,
					"ai-request-rewrite",
					fmt.Sprintf(
						`{"prompt":"rewrite","provider":"openai-compatible","auth":{"header":{"Authorization":"Bearer local-test"}},"override":{"endpoint":%q}}`,
						provider.URL,
					),
					ScopeRoute,
				),
				bindPluginForTest(
					"ai-aws-content-moderation",
					moderation,
					ScopeRoute,
					ResourceProvenance{Kind: ResourceRoute, ID: "moderation"},
				),
			}
			handler := NewRequestPipeline(
				bindings,
				nil,
			).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(w, r.Body) }))
			request, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(
					http.MethodPost,
					"/",
					strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
				),
				time.Now(),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if blocked {
				if response.Code != 400 || providerBody != "" {
					t.Fatalf("status=%d provider=%s", response.Code, providerBody)
				}
			} else if response.Code != 200 || response.Body.String() != "rewritten-from-llm" || !strings.Contains(providerBody, "decorator-first") {
				t.Fatalf("status=%d body=%s provider=%s", response.Code, response.Body.String(), providerBody)
			}
		})
	}
}

func TestAIRequestRewriteRunsBeforeRAGAccess(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"rewritten-first"}}]}`)
	}))
	defer provider.Close()
	rag := newExecutorRequestPlugin(
		"ai-rag",
		1060,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			body, _ := io.ReadAll(r.Body)
			if string(body) != "rewritten-first" {
				t.Errorf("RAG received %q before request rewrite", body)
			}
			w.WriteHeader(204)
			return base.StopRequest(r)
		},
	)
	bindings := []Binding{
		parityBinding(
			t,
			"ai-request-rewrite",
			fmt.Sprintf(
				`{"prompt":"rewrite","provider":"openai-compatible","auth":{"header":{"Authorization":"Bearer local"}},"override":{"endpoint":%q}}`,
				provider.URL,
			),
			ScopeRoute,
		),
		bindPluginForTest("ai-rag", rag, ScopeRoute, ResourceProvenance{Kind: ResourceRoute, ID: "rag"}),
	}
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("original")),
		time.Now(),
	)
	response := httptest.NewRecorder()
	NewRequestPipeline(
		bindings,
		nil,
	).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("RAG stop reached upstream") })).
		ServeHTTP(response, request)
	if response.Code != 204 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
