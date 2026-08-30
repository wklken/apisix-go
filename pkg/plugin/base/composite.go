package base

import "context"

// CompositeChildSpec describes one child selected by a composite plugin.
// Position is the stable structural path within the outer plugin config.
type CompositeChildSpec struct {
	Factory  string
	Config   map[string]any
	Position string
}

// PreparedCompositeChild owns one initialized, generation-bound child.
type PreparedCompositeChild interface {
	Factory() string
	Instance() any
	Close()
}

// CompositeChildPreparer constructs generation-owned children without exposing
// registry, descriptor, or binding internals to composite plugin packages.
type CompositeChildPreparer interface {
	Prepare(context.Context, ScopedSecretAccess, CompositeChildSpec) (PreparedCompositeChild, error)
}
