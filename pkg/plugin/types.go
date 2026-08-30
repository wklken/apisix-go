package plugin

import (
	"context"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
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

type ScopedSecretAccess = base.ScopedSecretAccess

type ScopedSecretMaterializer = base.ScopedSecretMaterializer

func MaterializeScopedPluginSecrets(
	ctx context.Context,
	baseScope secret.Scope,
	capability secret.GenerationSecrets,
	p Plugin,
) error {
	return base.MaterializeScopedPluginSecrets(ctx, baseScope, capability, p)
}
