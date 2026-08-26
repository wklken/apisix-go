package secret

import (
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestDescriptorFormatsOnlySourceAndDigest(t *testing.T) {
	value := newValue("super-secret-env-vault-ciphertext")
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Source() != capability.SecretPluginConfig || descriptor.Digest() != value.Digest() {
		t.Fatalf("descriptor identity = %q/%x", descriptor.Source(), descriptor.Digest())
	}
	const want = "plugin_config#sha256:a1a31f9f635acd668c8bb2ae360b619b0f07325c67c276081dd582150a5fca87"
	if got := descriptor.String(); got != want {
		t.Fatalf("descriptor string = %q, want %q", got, want)
	}
	for _, sensitive := range []string{"super-secret", "env", "vault", "ciphertext"} {
		if strings.Contains(descriptor.String(), sensitive) {
			t.Fatalf("descriptor leaked %q: %s", sensitive, descriptor)
		}
	}
	descriptorType := reflect.TypeFor[Descriptor]()
	for field := range descriptorType.Fields() {
		if field.IsExported() {
			t.Fatalf("descriptor exposes field %q", field.Name)
		}
	}
}

func TestDescriptorAcceptsEveryDeclarationSource(t *testing.T) {
	digest := sha256.Sum256([]byte("resolved"))
	for _, source := range []capability.SecretDeclarationSource{
		capability.SecretPluginConfig,
		capability.SecretPluginMetadata,
		capability.SecretConsumerConfig,
	} {
		descriptor, err := NewDescriptor(source, digest)
		if err != nil {
			t.Fatalf("NewDescriptor(%q) error = %v", source, err)
		}
		if descriptor.Source() != source || descriptor.Digest() != digest {
			t.Fatalf("NewDescriptor(%q) = %q/%x", source, descriptor.Source(), descriptor.Digest())
		}
	}
}

func TestDescriptorRejectsInvalidIdentityWithoutEchoingInput(t *testing.T) {
	digest := sha256.Sum256([]byte("resolved"))
	const unknown = capability.SecretDeclarationSource("$secret://vault/private/path")
	if _, err := NewDescriptor(unknown, digest); err == nil || strings.Contains(err.Error(), string(unknown)) {
		t.Fatalf("unknown source error = %v, want redacted rejection", err)
	}
	if _, err := NewDescriptor(capability.SecretPluginConfig, [32]byte{}); err == nil {
		t.Fatal("zero digest error = nil")
	}
	if _, err := (Value{}).Descriptor(capability.SecretPluginConfig); err == nil {
		t.Fatal("zero value descriptor error = nil")
	}
}
