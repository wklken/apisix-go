package tencent_cloud_cls

import "testing"

func TestGlobalTagOverwritesEntryField(t *testing.T) {
	contents, _, _ := normalizeLog(
		map[string]any{"environment": "from-entry", "route_id": "1"},
		map[string]string{"environment": "from-tag"},
	)
	var environments []string
	for _, content := range contents {
		if content.key == "environment" {
			environments = append(environments, content.value)
		}
	}
	if len(environments) != 1 || environments[0] != "from-tag" {
		t.Fatalf("environment contents=%v, want one overwritten field", environments)
	}
}
