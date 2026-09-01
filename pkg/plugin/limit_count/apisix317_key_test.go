package limit_count

import (
	"hash/crc32"
	"strconv"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestBuildLocalKeyMatchesAPISIX317EffectiveConfigIdentity(t *testing.T) {
	ctx := base.APISIXPluginContext{
		Provider:          "etcd",
		EtcdPrefix:        "/apisix",
		SourceKind:        "route",
		SourceID:          "route-1",
		SourceResourceKey: "/apisix/routes/route-1",
	}

	got, err := BuildLocalKey(ctx, map[string]any{
		"count":       2,
		"time_window": 60,
	}, "alice")
	if err != nil {
		t.Fatalf("BuildLocalKey() error = %v", err)
	}

	canonical := `{"_meta":[],"allow_degradation":false,"count":2,"key":"remote_addr","key_type":"var","policy":"local","rejected_code":503,"show_limit_quota_header":true,"time_window":60}`
	want := ctx.SourceResourceKey + ":" +
		strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(canonical))), 10) + ":alice"
	if got != want {
		t.Fatalf("BuildLocalKey() = %q, want %q", got, want)
	}
}

func TestBuildLocalKeyGroupBypassesParentAndConfigVersion(t *testing.T) {
	got, err := BuildLocalKey(base.APISIXPluginContext{}, map[string]any{
		"group":       "shared",
		"count":       2,
		"time_window": 60,
	}, "alice")
	if err != nil {
		t.Fatalf("BuildLocalKey() error = %v", err)
	}
	if got != "shared:alice" {
		t.Fatalf("BuildLocalKey() = %q, want shared:alice", got)
	}
}

func TestBuildLocalKeyIncludesWorkflowVIDInVersionAndSuffix(t *testing.T) {
	ctx := base.APISIXPluginContext{
		Provider:    "standalone",
		SourceKind:  "route",
		SourceID:    "route-1",
		WorkflowVID: 3,
	}

	got, err := BuildLocalKey(ctx, map[string]any{
		"count":       2,
		"time_window": 60,
	}, "alice")
	if err != nil {
		t.Fatalf("BuildLocalKey() error = %v", err)
	}

	canonical := `{"_meta":[],"_vid":3,"allow_degradation":false,"count":2,"key":"remote_addr","key_type":"var","policy":"local","rejected_code":503,"show_limit_quota_header":true,"time_window":60}`
	want := "/routes/route-1:" +
		strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(canonical))), 10) + ":alice:3"
	if got != want {
		t.Fatalf("BuildLocalKey() = %q, want %q", got, want)
	}
}

func TestBuildLocalKeyWithVIDSupportsAIStringIdentity(t *testing.T) {
	ctx := base.APISIXPluginContext{
		Provider:   "standalone",
		SourceKind: "route",
		SourceID:   "route-1",
	}
	config := map[string]any{
		"count":       2,
		"time_window": 60,
	}

	got, err := BuildLocalKeyWithVID(ctx, config, "alice", "ai-rate-limiting#global")
	if err != nil {
		t.Fatalf("BuildLocalKeyWithVID() error = %v", err)
	}
	canonical := `{"_meta":[],"_vid":"ai-rate-limiting#global","allow_degradation":false,"count":2,"key":"remote_addr","key_type":"var","policy":"local","rejected_code":503,"show_limit_quota_header":true,"time_window":60}`
	want := "/routes/route-1:" +
		strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(canonical))), 10) +
		":alice:ai-rate-limiting#global"
	if got != want {
		t.Fatalf("BuildLocalKeyWithVID() = %q, want %q", got, want)
	}
}

func TestBuildLocalKeyRejectsMissingParentForNonGroup(t *testing.T) {
	if _, err := BuildLocalKey(base.APISIXPluginContext{}, map[string]any{
		"count":       2,
		"time_window": 60,
	}, "alice"); err == nil {
		t.Fatal("BuildLocalKey() error = nil, want missing parent error")
	}
}
