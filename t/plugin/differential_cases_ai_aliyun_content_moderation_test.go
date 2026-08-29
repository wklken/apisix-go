package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIAliyunContentModerationCasesCoverPinnedMissingAIInstance(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	want := []DifferentialCase{{
		Name:    "ai-aliyun-content-moderation-missing-ai-instance",
		Plugin:  "ai-aliyun-content-moderation",
		RouteID: "differential-ai-aliyun-missing-instance",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  "differential-ai-aliyun-missing-instance",
				"uri": "/chat",
				"plugins": map[string]any{
					"ai-aliyun-content-moderation": map[string]any{
						"endpoint":          "http://" + differentialFixturePlaceholder,
						"region_id":         "cn-shanghai",
						"access_key_id":     "fake-key-id",
						"access_key_secret": "fake-key-secret",
						"risk_level_bar":    "high",
						"check_request":     true,
						"fail_mode":         "error",
					},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/chat",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"X-AI-Fixture": "aliyun/chat-with-harmful.json",
			},
			Body: `{"prompt": "What is 1+1?"}`,
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "not_applicable",
	}}

	got := differentialAIAliyunContentModerationCases()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("differentialAIAliyunContentModerationCases() = %#v, want %#v", got, want)
	}
	if got[0].ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", got[0].ComparisonPolicy)
	}
}
