package plugin

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin interface {
	Init() error
	PostInit() error
	Handler(next http.Handler) http.Handler
	Config() any
	GetSchema() string
	GetMetadataSchema() string
	GetPriority() int
	GetName() string
}

// SecretMaterializer resolves generation-owned credentials after schema
// decoding and before PostInit. Implementations must retain only redacted
// descriptors in their public config.
type SecretMaterializer = base.SecretMaterializer

// MaterializePluginSecrets runs the optional pre-PostInit secret phase.
func MaterializePluginSecrets(p Plugin) error {
	return base.MaterializePluginSecrets(p)
}
