package workflow

import (
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

// APISIX 3.17 workflow.t TEST 14 at
// 9ef2ecab67f652d38365049613610ef649bb4ad0 rejects limit-count.group.
func TestAPISIX317WorkflowTest14RejectsLimitCountGroup(t *testing.T) {
	config := map[string]any{
		"rules": []any{map[string]any{
			"case": []any{[]any{"uri", "==", "/hello"}},
			"actions": []any{[]any{
				"limit-count",
				map[string]any{
					"count":         2,
					"time_window":   60,
					"rejected_code": 503,
					"group":         "services_1",
				},
			}},
		}},
	}

	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(config, plugin.GetSchema()); err != nil {
		t.Fatalf("outer workflow schema rejected APISIX TEST 14 shape before action validation: %v", err)
	}
	if err := util.Parse(config, plugin.Config()); err != nil {
		t.Fatalf("parse APISIX TEST 14 config: %v", err)
	}
	err := plugin.ValidatePreMaterialization()
	if err == nil || !strings.Contains(err.Error(), "group is not supported") {
		t.Fatalf("ValidatePreMaterialization() error = %v, want limit-count.group rejection", err)
	}
}
