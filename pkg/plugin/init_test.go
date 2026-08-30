package plugin

import (
	"net/http"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// pluginNameAliases maps registry keys to the plugin names they register,
// which differ for historical compatibility.
var pluginNameAliases = map[string]string{
	"otel": "opentelemetry",
}

func TestNewConstructsEveryRegisteredPlugin(t *testing.T) {
	for name, registered := range pluginRegistry {
		t.Run(name, func(t *testing.T) {
			plugin := registered.create()
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
	for _, name := range []string{"", "unknown-plugin", "unknown-plugin-misspelled"} {
		if got := New(name, base.Dependencies{}); got != nil {
			t.Fatalf("New(%q) = %v, want nil", name, got)
		}
	}
}

func TestNewRejectsRequestContext(t *testing.T) {
	if got := New("request-context", base.Dependencies{}); got != nil {
		t.Fatalf("New(request-context) = %T, want nil", got)
	}
}

func TestNewInjectsDependenciesIntoEachPluginInstance(t *testing.T) {
	leftConfig := &config.EffectiveConfig{}
	rightConfig := &config.EffectiveConfig{}

	left := New("echo", base.Dependencies{Config: leftConfig})
	right := New("echo", base.Dependencies{Config: rightConfig})
	if left == nil || right == nil || left == right {
		t.Fatalf("New(echo) = (%v, %v), want distinct plugin instances", left, right)
	}
	leftReceiver, ok := left.(interface {
		StaticConfig() *config.EffectiveConfig
	})
	if !ok {
		t.Fatal("New(echo) does not expose injected static configuration")
	}
	rightReceiver, ok := right.(interface {
		StaticConfig() *config.EffectiveConfig
	})
	if !ok {
		t.Fatal("second New(echo) does not expose injected static configuration")
	}
	if leftReceiver.StaticConfig() != leftConfig || rightReceiver.StaticConfig() != rightConfig {
		t.Fatal("New(echo) shared or replaced instance-scoped dependencies")
	}
}

func TestNewPanicsWhenRegisteredPluginCannotReceiveDependencies(t *testing.T) {
	const name = "test-plugin-without-base"
	pluginRegistry[name] = registration{create: func() Plugin { return &pluginWithoutBase{} }}
	t.Cleanup(func() { delete(pluginRegistry, name) })

	defer func() {
		if got := recover(); got != "registered plugin does not embed base.BasePlugin" {
			t.Fatalf("New(%q) panic = %v, want stable invariant message", name, got)
		}
	}()
	New(name, base.Dependencies{})
	t.Fatalf("New(%q) did not panic", name)
}

func TestNewReturnsRegisteredPlugin(t *testing.T) {
	for name := range pluginRegistry {
		if got := New(name, base.Dependencies{}); got == nil {
			t.Fatalf("New(%q) = nil, want a plugin", name)
		}
	}
}

func TestNewPreservesHistoricalFactoryAliases(t *testing.T) {
	for factory, wantName := range map[string]string{
		"otel":          "opentelemetry",
		"opentelemetry": "opentelemetry",
	} {
		plugin := New(factory, base.Dependencies{})
		if plugin == nil {
			t.Fatalf("New(%q) = nil, want registered alias", factory)
		}
		if err := plugin.Init(); err != nil {
			t.Fatalf("New(%q).Init() error = %v", factory, err)
		}
		if got := plugin.GetName(); got != wantName {
			t.Fatalf("New(%q).GetName() = %q, want %q", factory, got, wantName)
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

type pluginWithoutBase struct{}

func (p *pluginWithoutBase) Init() error                            { return nil }
func (p *pluginWithoutBase) PostInit() error                        { return nil }
func (p *pluginWithoutBase) Handler(next http.Handler) http.Handler { return next }
func (p *pluginWithoutBase) Config() any                            { return nil }
func (p *pluginWithoutBase) GetSchema() string                      { return "" }
func (p *pluginWithoutBase) GetMetadataSchema() string              { return "" }
func (p *pluginWithoutBase) GetPriority() int                       { return 0 }
func (p *pluginWithoutBase) GetName() string                        { return "test-plugin-without-base" }
