package generation

import "context"

// DesiredApplier applies one canonical provider batch and returns only after
// the durable publication acknowledgement is available.
type DesiredApplier interface {
	Apply(context.Context, DesiredBatch) (Acknowledgement, error)
}
