package secret

import (
	"encoding/hex"
	"errors"

	"github.com/wklken/apisix-go/pkg/capability"
)

var (
	errDescriptorSourceInvalid = errors.New("secret descriptor source is invalid")
	errDescriptorDigestInvalid = errors.New("secret descriptor digest is invalid")
)

// Descriptor identifies materialized secret content without retaining the
// source reference or resolved plaintext.
type Descriptor struct {
	source capability.SecretDeclarationSource
	digest [32]byte
}

func NewDescriptor(
	source capability.SecretDeclarationSource,
	digest [32]byte,
) (Descriptor, error) {
	if !validDescriptorSource(source) {
		return Descriptor{}, errDescriptorSourceInvalid
	}
	if digest == ([32]byte{}) {
		return Descriptor{}, errDescriptorDigestInvalid
	}
	return Descriptor{source: source, digest: digest}, nil
}

func (value Value) Descriptor(source capability.SecretDeclarationSource) (Descriptor, error) {
	return NewDescriptor(source, value.digest)
}

func (descriptor Descriptor) Source() capability.SecretDeclarationSource { return descriptor.source }

func (descriptor Descriptor) Digest() [32]byte { return descriptor.digest }

func (descriptor Descriptor) String() string {
	return string(descriptor.source) + "#sha256:" + hex.EncodeToString(descriptor.digest[:])
}

func validDescriptorSource(source capability.SecretDeclarationSource) bool {
	switch source {
	case capability.SecretPluginConfig,
		capability.SecretPluginMetadata,
		capability.SecretConsumerConfig:
		return true
	default:
		return false
	}
}
