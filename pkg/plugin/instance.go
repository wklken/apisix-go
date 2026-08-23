package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type InstanceKey struct {
	Factory      string
	Scope        Scope
	Owner        ResourceProvenance
	ConfigDigest [32]byte
}

// InstanceIdentityInput contains only configuration that changes the behavior
// of one materialized plugin instance. Ordering-only and enablement metadata
// deliberately remain outside the identity.
type InstanceIdentityInput struct {
	PluginConfig  any `json:"plugin_config"`
	Filter        any `json:"filter,omitempty"`
	ErrorResponse any `json:"error_response,omitempty"`
}

func NewInstanceKey(
	descriptor Descriptor,
	scope Scope,
	owner ResourceProvenance,
	identity InstanceIdentityInput,
) (InstanceKey, error) {
	if descriptor.Factory == "" {
		return InstanceKey{}, fmt.Errorf("plugin instance key: factory is required")
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return InstanceKey{}, fmt.Errorf(
			"plugin instance key %q: canonicalize config: %w",
			descriptor.Factory,
			err,
		)
	}
	return InstanceKey{
		Factory:      descriptor.Factory,
		Scope:        scope,
		Owner:        owner,
		ConfigDigest: sha256.Sum256(encoded),
	}, nil
}

func (k InstanceKey) String() string {
	return fmt.Sprintf(
		"%s/%d/%s/%s/%s",
		k.Factory,
		k.Scope,
		k.Owner.Kind,
		k.Owner.ID,
		hex.EncodeToString(k.ConfigDigest[:]),
	)
}
