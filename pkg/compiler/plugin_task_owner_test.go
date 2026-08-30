package compiler

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
)

func pluginTaskOwnerGoldenInstance() plugin.InstanceKey {
	var digest [32]byte
	for index := range digest {
		digest[index] = byte(index + 32)
	}
	return plugin.InstanceKey{
		Factory:      "http-logger",
		Generation:   42,
		Scope:        plugin.ScopeRoute,
		Owner:        plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "r/1\n"},
		ConfigDigest: digest,
	}
}

func TestPluginTaskOwnerPrefixUsesCanonicalBoundedIdentity(t *testing.T) {
	const want = "plugin/http-logger/2bf890a628acc49afd1415d01d9466fd507b81c5bde0fcbf48501b48689196ec"
	got, err := pluginTaskOwnerPrefix(pluginTaskOwnerGoldenInstance())
	if err != nil || got != want {
		t.Fatalf("pluginTaskOwnerPrefix() = (%q, %v), want (%q, nil)", got, err, want)
	}
}

func TestPluginTaskOwnerPrefixDoesNotExposeOrExpandResourceIdentity(t *testing.T) {
	instance := pluginTaskOwnerGoldenInstance()
	instance.Factory = "HTTP/" + strings.Repeat("very-long_", 32) + "\nLogger"
	instance.Owner.ID = strings.Repeat("route/with/newline\n", 4<<10)

	prefix, err := pluginTaskOwnerPrefix(instance)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prefix, instance.Factory) ||
		strings.Contains(prefix, instance.Owner.ID) ||
		strings.Contains(prefix, "\n") {
		t.Fatalf("prefix exposed raw identity: %q", prefix)
	}
	if strings.Count(prefix, "/") != 2 {
		t.Fatalf("prefix slash count = %d, want 2: %q", strings.Count(prefix, "/"), prefix)
	}
	parts := strings.Split(prefix, "/")
	if len(parts[1]) == 0 || len(parts[1]) > pluginTaskOwnerFactoryMaxLen ||
		!regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$`).MatchString(parts[1]) {
		t.Fatalf("sanitized factory = %q, want frozen 1-48 byte ASCII grammar", parts[1])
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(parts[2]) {
		t.Fatalf("digest = %q, want 64 lower-case hexadecimal bytes", parts[2])
	}
	if len(prefix) > 120 {
		t.Fatalf("prefix length = %d, want at most 120", len(prefix))
	}
}

func TestSanitizePluginTaskOwnerFactoryUsesFrozenASCIIRules(t *testing.T) {
	tests := []struct {
		name    string
		factory string
		want    string
	}{
		{
			name:    "collapse consecutive disallowed bytes",
			factory: "HTTP/// \tLogger",
			want:    "http-logger",
		},
		{
			name:    "trim leading and trailing separators",
			factory: "--/HTTP Logger/_--",
			want:    "http-logger",
		},
		{
			name:    "trim separator at 48 byte truncation boundary",
			factory: "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijk-tail",
			want:    "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijk",
		},
		{
			name:    "fallback when every byte is disallowed",
			factory: "///\n__",
			want:    "unknown",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizePluginTaskOwnerFactory(test.factory); got != test.want {
				t.Fatalf("sanitizePluginTaskOwnerFactory(%q) = %q, want %q", test.factory, got, test.want)
			}
		})
	}
}

func TestPluginTaskOwnerPrefixIncludesEveryInstanceKeyField(t *testing.T) {
	golden := pluginTaskOwnerGoldenInstance()
	want, err := pluginTaskOwnerPrefix(golden)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*plugin.InstanceKey)
	}{
		{name: "factory", mutate: func(key *plugin.InstanceKey) { key.Factory = "http-logger-2" }},
		{name: "generation", mutate: func(key *plugin.InstanceKey) { key.Generation++ }},
		{name: "scope", mutate: func(key *plugin.InstanceKey) { key.Scope = plugin.ScopeConsumer }},
		{name: "owner kind", mutate: func(key *plugin.InstanceKey) { key.Owner.Kind = plugin.ResourceService }},
		{name: "owner ID", mutate: func(key *plugin.InstanceKey) { key.Owner.ID = "r/2\n" }},
		{name: "config digest", mutate: func(key *plugin.InstanceKey) { key.ConfigDigest[11]++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := golden
			test.mutate(&changed)
			got, err := pluginTaskOwnerPrefix(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("prefix omitted changed %s field: %q", test.name, got)
			}
		})
	}
}

func TestPluginTaskOwnerPrefixSeparatesFactoriesWithSameReadableSegment(t *testing.T) {
	first := pluginTaskOwnerGoldenInstance()
	first.Factory = "HTTP Logger"
	second := first
	second.Factory = "http/logger"

	firstPrefix, err := pluginTaskOwnerPrefix(first)
	if err != nil {
		t.Fatal(err)
	}
	secondPrefix, err := pluginTaskOwnerPrefix(second)
	if err != nil {
		t.Fatal(err)
	}
	firstParts := strings.Split(firstPrefix, "/")
	secondParts := strings.Split(secondPrefix, "/")
	if firstParts[1] != "http-logger" || secondParts[1] != "http-logger" {
		t.Fatalf("readable factory segments = %q/%q, want http-logger", firstParts[1], secondParts[1])
	}
	if firstPrefix == secondPrefix || firstParts[2] == secondParts[2] {
		t.Fatalf("raw factory identity collided: %q / %q", firstPrefix, secondPrefix)
	}
}

func TestPluginTaskOwnerPrefixRejectsIncompleteIdentity(t *testing.T) {
	valid := pluginTaskOwnerGoldenInstance()
	tests := []struct {
		name   string
		mutate func(*plugin.InstanceKey)
	}{
		{name: "blank factory", mutate: func(key *plugin.InstanceKey) { key.Factory = " \t" }},
		{name: "zero generation", mutate: func(key *plugin.InstanceKey) { key.Generation = 0 }},
		{name: "out of range scope", mutate: func(key *plugin.InstanceKey) { key.Scope = plugin.ScopeConsumer + 1 }},
		{name: "empty owner kind", mutate: func(key *plugin.InstanceKey) { key.Owner.Kind = "" }},
		{name: "empty owner ID", mutate: func(key *plugin.InstanceKey) { key.Owner.ID = "" }},
		{name: "zero config digest", mutate: func(key *plugin.InstanceKey) { key.ConfigDigest = [32]byte{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := valid
			test.mutate(&instance)
			prefix, err := pluginTaskOwnerPrefix(instance)
			if prefix != "" || !errors.Is(err, errPluginTaskOwnerIdentity) {
				t.Fatalf("pluginTaskOwnerPrefix() = (%q, %v), want (empty, identity error)", prefix, err)
			}
		})
	}
}
