package plugin

import (
	"testing"
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
