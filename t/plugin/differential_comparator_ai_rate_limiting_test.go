package pluginintegration

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIRateLimitingWindowComparison(t *testing.T) {
	spec, candidate, oracle := differentialAIRateLimitingWindowTestObservations()
	candidateBefore := cloneDifferentialObservation(candidate)
	oracleBefore := cloneDifferentialObservation(oracle)

	passed, diff, err := compareDifferentialCaseObservations(
		spec,
		candidate,
		oracle,
		testNormalizationPolicy(),
	)
	if err != nil {
		t.Fatalf("compareDifferentialCaseObservations() error = %v", err)
	}
	if !passed || diff != "" {
		t.Fatalf("AI rate-limiting window rejected: passed=%t diff=%q", passed, diff)
	}
	if !reflect.DeepEqual(candidate, candidateBefore) || !reflect.DeepEqual(oracle, oracleBefore) {
		t.Fatal("AI rate-limiting comparison mutated caller observations")
	}
}

func TestDifferentialAIRateLimitingWindowRejectsSemanticDifferences(t *testing.T) {
	assertRejected := func(
		t *testing.T,
		edit func(*DifferentialCase, *DifferentialObservation, *DifferentialObservation),
	) {
		t.Helper()
		spec, candidate, oracle := differentialAIRateLimitingWindowTestObservations()
		edit(&spec, &candidate, &oracle)
		passed, _, _ := compareDifferentialCaseObservations(
			spec,
			candidate,
			oracle,
			testNormalizationPolicy(),
		)
		if passed {
			t.Fatal("semantic difference was normalized")
		}
	}

	for _, value := range []string{"0", "01", "+1", "61", "not-an-integer"} {
		t.Run("invalid reset "+value, func(t *testing.T) {
			assertRejected(
				t,
				func(_ *DifferentialCase, candidate *DifferentialObservation, _ *DifferentialObservation) {
					candidate.Steps[0].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{value}
				},
			)
		})
	}
	t.Run("reset increases", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[0].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{"59"}
			oracle.Steps[1].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{"60"}
		})
	})
	t.Run("first reset does not expose the full window", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[0].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{"59"}
		})
	})
	t.Run("remaining changes", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.Steps[1].Headers["X-AI-RateLimit-Remaining-ai-proxy-openai"] = []string{"19"}
		})
	})
	t.Run("oracle upstream owns a nonempty query", func(t *testing.T) {
		assertRejected(t, func(_ *DifferentialCase, _ *DifferentialObservation, oracle *DifferentialObservation) {
			oracle.UpstreamCalls[0].Path = "/v1/chat/completions?x=1"
		})
	})
	t.Run("policy is used by another plugin", func(t *testing.T) {
		assertRejected(t, func(spec *DifferentialCase, _ *DifferentialObservation, _ *DifferentialObservation) {
			spec.Plugin = "limit-count"
		})
	})
}

func differentialAIRateLimitingWindowTestObservations() (
	DifferentialCase,
	DifferentialObservation,
	DifferentialObservation,
) {
	const responseBody = `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"1 + 1 = 2.","role":"assistant"}}],"created":1723780938,"id":"chatcmpl-9wiSIg5LYrrpxwsr2PubSQnbtod1P","model":"gpt-35-turbo-instruct","object":"chat.completion","system_fingerprint":"fp_abc28019ad","usage":{"completion_tokens":5,"prompt_tokens":8,"total_tokens":10}}`
	const rejectBody = "{\"error_msg\":\"rate limit exceeded\"}\n"
	spec := differentialAIRateLimitingCases()[0]
	spec.ComparisonPolicy = "ai-rate-limiting-window"

	step := func(index int, reset string) DifferentialStepObservation {
		status := http.StatusOK
		decision := "allow"
		body := responseBody
		if index == 3 {
			status = http.StatusForbidden
			decision = "deny"
			body = rejectBody
		}
		return DifferentialStepObservation{
			Status: status,
			Headers: map[string][]string{
				"Content-Type": {
					map[bool]string{true: "text/plain; charset=utf-8", false: "application/json"}[index == 3],
				},
				"X-AI-RateLimit-Limit-ai-proxy-openai":     {"30"},
				"X-AI-RateLimit-Remaining-ai-proxy-openai": {fmt.Sprint(max(30-index*10, 0))},
				"X-AI-RateLimit-Reset-ai-proxy-openai":     {reset},
			},
			Body: body, Host: "gateway.example.test", SecurityDecision: decision,
		}
	}
	candidate := DifferentialObservation{
		Steps:           []DifferentialStepObservation{step(0, "60"), step(1, "60"), step(2, "60"), step(3, "60")},
		UpstreamFixture: "primary", UpstreamAddress: "127.0.0.1:52429",
		Upstream: DifferentialUpstreamObservation{
			Received: true, Fixture: "primary", Method: http.MethodPost,
			Path: "/v1/chat/completions", Host: "127.0.0.1:52429",
		},
	}
	for range 3 {
		candidate.UpstreamCalls = append(candidate.UpstreamCalls, candidate.Upstream)
	}
	oracle := cloneDifferentialObservation(candidate)
	oracle.UpstreamAddress = "127.0.0.1:1980"
	oracle.Upstream.Host = "127.0.0.1:1980"
	oracle.Upstream.Path = "/v1/chat/completions?"
	for index := range oracle.UpstreamCalls {
		oracle.UpstreamCalls[index].Host = "127.0.0.1:1980"
		oracle.UpstreamCalls[index].Path = "/v1/chat/completions?"
	}
	for index := 1; index < len(oracle.Steps)-1; index++ {
		oracle.Steps[index].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{"59"}
	}
	oracle.Steps[len(oracle.Steps)-1].Headers["X-AI-RateLimit-Reset-ai-proxy-openai"] = []string{"58"}
	return spec, candidate, oracle
}
