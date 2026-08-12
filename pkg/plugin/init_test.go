package plugin

import (
	"net/http"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// pluginNameAliases maps registry keys to the plugin names they register,
// which differ for historical compatibility.
var pluginNameAliases = map[string]string{
	"otel":            "opentelemetry",
	"request-context": "request_context",
}

func TestNewConstructsEveryRegisteredPlugin(t *testing.T) {
	for name, factory := range pluginRegistry {
		t.Run(name, func(t *testing.T) {
			plugin := factory()
			if plugin == nil {
				t.Fatal("factory returned nil")
			}
			if err := plugin.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			want := name
			if renamed, ok := pluginNameAliases[name]; ok {
				want = renamed
			}
			if got := plugin.GetName(); got != want {
				t.Fatalf("GetName() = %q, want %q", got, want)
			}
		})
	}
}

func TestNewRejectsUnknownNames(t *testing.T) {
	for _, name := range []string{"", "unknown-plugin", "request-context-misspelled"} {
		if got := New(name); got != nil {
			t.Fatalf("New(%q) = %v, want nil", name, got)
		}
	}
}

func TestNewReturnsRegisteredPlugin(t *testing.T) {
	for name := range pluginRegistry {
		if got := New(name); got == nil {
			t.Fatalf("New(%q) = nil, want a plugin", name)
		}
	}
}

func TestPlan16CapabilityRegistryCoversExactHTTPIdentities(t *testing.T) {
	wantHTTP := []string{
		"ai-aliyun-content-moderation", "ai-proxy", "ai-proxy-multi", "ai-rate-limiting",
		"aws-lambda", "azure-functions", "brotli", "cors", "dubbo-proxy", "fault-injection",
		"grpc-transcode", "grpc-web", "gzip", "http-dubbo", "kafka-proxy", "mcp-bridge",
		"mocking", "openfunction", "openwhisk", "proxy-buffering", "public-api", "redirect",
	}
	got := make([]string, 0, len(wantHTTP))
	for _, identity := range wantHTTP {
		if _, ok := pluginRegistry[identity]; !ok {
			t.Fatalf("Plan16 identity %q is not a registered factory", identity)
		}
		if _, ok := ResponseCapabilityFor(identity); !ok {
			t.Fatalf("Plan16 identity %q has no response capability", identity)
		}
		got = append(got, identity)
	}
	slices.Sort(got)
	want := append([]string(nil), wantHTTP...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Plan16 HTTP identities = %v, want %v", got, want)
	}
	mqtt, ok := ResponseCapabilityFor("mqtt-proxy")
	if !ok || !mqtt.SeparateSubsystem || mqtt.ExclusiveProtocol != ProtocolMQTT {
		t.Fatalf("mqtt-proxy capability = %#v/%v, want HTTP-excluded MQTT subsystem", mqtt, ok)
	}
}

func TestRequestStageRegistryFactoriesMatchConstructors(t *testing.T) {
	for name := range requestStageRegistry {
		t.Run(name, func(t *testing.T) {
			factory, ok := pluginRegistry[name]
			if !ok {
				t.Fatalf("request-stage factory %q is not registered", name)
			}
			plugin := factory()
			if plugin == nil {
				t.Fatal("request-stage factory returned nil")
			}
			if err := plugin.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			want := name
			if renamed, ok := pluginNameAliases[name]; ok {
				want = renamed
			}
			if got := plugin.GetName(); got != want {
				t.Fatalf("GetName() = %q, want %q", got, want)
			}
		})
	}
}

type chainTestPlugin struct {
	base.BasePlugin
}

func (p *chainTestPlugin) Init() error     { return nil }
func (p *chainTestPlugin) PostInit() error { return nil }
func (p *chainTestPlugin) Handler(next http.Handler) http.Handler {
	return next
}
func (p *chainTestPlugin) Config() any { return nil }

func TestBuildPluginChainDoesNotMutateCallerSlice(t *testing.T) {
	low := &chainTestPlugin{}
	low.Name = "low"
	low.SetPriority(10)
	high := &chainTestPlugin{}
	high.Name = "high"
	high.SetPriority(100)

	plugins := []Plugin{low, high}
	BuildPluginChain(plugins...)

	if plugins[0] != low || plugins[1] != high {
		t.Fatalf(
			"BuildPluginChain mutated the caller slice: got [%s %s], want [low high]",
			plugins[0].GetName(),
			plugins[1].GetName(),
		)
	}
}
