package runtime

import (
	"errors"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/secret"
)

type RuntimeDependencies struct {
	Config    *config.EffectiveConfig
	Secrets   secret.Materializer
	Resources *ResourceRegistry
	Tasks     *TaskRegistry
}

func (dependencies RuntimeDependencies) Validate() error {
	if dependencies.Config == nil {
		return errors.New("runtime dependencies: effective config is required")
	}
	if dependencies.Secrets == nil {
		return errors.New("runtime dependencies: secret materializer is required")
	}
	if dependencies.Resources == nil {
		return errors.New("runtime dependencies: resource registry is required")
	}
	if dependencies.Tasks == nil {
		return errors.New("runtime dependencies: task registry is required")
	}
	return nil
}
