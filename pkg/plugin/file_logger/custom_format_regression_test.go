package file_logger

import (
	"testing"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestCustomLogFormatDoesNotInjectBodies(t *testing.T) {
	for _, format := range []map[string]any{{}, {"case": "custom"}, {"request": map[string]any{"body": "$request_body"}}} {
		p := newTestPlugin(
			t,
			Config{Path: t.TempDir() + "/access.log", LogFormat: format, IncludeReqBody: true, IncludeRespBody: true},
		)
		fields := p.buildSnapshotFields(
			base.LogSnapshot{
				Request:  apisixlog.RequestLogSnapshot{Body: []byte("request")},
				Response: apisixlog.ResponseLogSnapshot{Body: []byte("response")},
			},
		)
		if fields["response"] != nil {
			t.Fatalf("custom response = %#v", fields["response"])
		}
		if format["request"] == nil {
			if fields["request"] != nil {
				t.Fatalf("custom request = %#v", fields["request"])
			}
		} else if fields["request"].(map[string]any)["body"] != "request" {
			t.Fatalf("explicit request body = %#v", fields["request"])
		}
	}
}

func TestDefaultLogReportsAPISIXVersion(t *testing.T) {
	fields := snapshotDefaultLogFields(base.LogSnapshot{})
	if fields["server"].(map[string]any)["version"] != "3.17.0" {
		t.Fatalf("server = %#v", fields["server"])
	}
}

func TestEmptyMetadataFormatKeepsDefaultBodies(t *testing.T) {
	p := newTestPluginWithMetadata(
		t,
		Config{Path: t.TempDir() + "/access.log", IncludeReqBody: true, IncludeRespBody: true},
		map[string]any{"log_format": map[string]any{}},
	)
	fields := p.buildSnapshotFields(
		base.LogSnapshot{
			Request:  apisixlog.RequestLogSnapshot{Body: []byte("request")},
			Response: apisixlog.ResponseLogSnapshot{Body: []byte("response")},
		},
	)
	for field, want := range map[string]string{"request": "request", "response": "response"} {
		nested, ok := fields[field].(map[string]any)
		if !ok || nested["body"] != want {
			t.Fatalf("default %s = %#v", field, fields[field])
		}
	}
	if fields["server"] == nil {
		t.Fatal("empty metadata removed default log fields")
	}
}
