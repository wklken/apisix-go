package runtime

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestRuntimeDependenciesValidateRejectsMissingDependencies(t *testing.T) {
	complete := RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   secret.NewScopedMaterializer(nil),
		Resources: NewResourceRegistry(),
		Tasks:     NewTaskRegistry(context.Background(), nil),
	}

	tests := []struct {
		name         string
		dependencies RuntimeDependencies
		want         string
	}{
		{
			name: "effective config",
			dependencies: RuntimeDependencies{
				Secrets:   complete.Secrets,
				Resources: complete.Resources,
				Tasks:     complete.Tasks,
			},
			want: "runtime dependencies: effective config is required",
		},
		{
			name: "secret materializer",
			dependencies: RuntimeDependencies{
				Config:    complete.Config,
				Resources: complete.Resources,
				Tasks:     complete.Tasks,
			},
			want: "runtime dependencies: secret materializer is required",
		},
		{
			name: "resource registry",
			dependencies: RuntimeDependencies{
				Config:  complete.Config,
				Secrets: complete.Secrets,
				Tasks:   complete.Tasks,
			},
			want: "runtime dependencies: resource registry is required",
		},
		{
			name: "task registry",
			dependencies: RuntimeDependencies{
				Config:    complete.Config,
				Secrets:   complete.Secrets,
				Resources: complete.Resources,
			},
			want: "runtime dependencies: task registry is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dependencies.Validate()
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRuntimeDependenciesValidateReportsFirstMissingDependencyInOrder(t *testing.T) {
	configOnly := RuntimeDependencies{
		Config: &config.EffectiveConfig{},
	}
	configAndSecrets := configOnly
	configAndSecrets.Secrets = secret.NewScopedMaterializer(nil)
	configSecretsAndResources := configAndSecrets
	configSecretsAndResources.Resources = NewResourceRegistry()

	tests := []struct {
		name         string
		dependencies RuntimeDependencies
		want         string
	}{
		{
			name: "zero value reports effective config first",
			want: "runtime dependencies: effective config is required",
		},
		{
			name:         "config only reports secret materializer next",
			dependencies: configOnly,
			want:         "runtime dependencies: secret materializer is required",
		},
		{
			name:         "config and secrets report resource registry next",
			dependencies: configAndSecrets,
			want:         "runtime dependencies: resource registry is required",
		},
		{
			name:         "config secrets and resources report task registry last",
			dependencies: configSecretsAndResources,
			want:         "runtime dependencies: task registry is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.dependencies.Validate()
			if err == nil || err.Error() != tt.want {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestRuntimeDependenciesValidateAcceptsCompleteDependencies(t *testing.T) {
	dependencies := RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   secret.NewScopedMaterializer(nil),
		Resources: NewResourceRegistry(),
		Tasks:     NewTaskRegistry(context.Background(), nil),
	}

	if err := dependencies.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
