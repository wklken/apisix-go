package runtime

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/secret"
)

type testAttemptRegistration struct{}

func (testAttemptRegistration) AttemptID() secret.AttemptID {
	return secret.AttemptID{1}
}

func (testAttemptRegistration) Materialize(context.Context, secret.Scope, string) (secret.Value, error) {
	panic("runtime dependency validation materialized a secret")
}

func (testAttemptRegistration) Close(context.Context) error {
	return nil
}

func testGenerationCapability(t *testing.T) secret.GenerationCapability {
	t.Helper()
	capability, err := secret.NewGenerationCapability(testAttemptRegistration{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

func TestRuntimeDependenciesValidateRejectsMissingDependencies(t *testing.T) {
	complete := RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   testGenerationCapability(t),
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
			want: "runtime dependencies: generation secret capability is required",
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
	configAndSecrets.Secrets = testGenerationCapability(t)
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
			want:         "runtime dependencies: generation secret capability is required",
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

func TestRuntimeDependenciesValidateAcceptsEmptyMetadataView(t *testing.T) {
	dependencies := RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   testGenerationCapability(t),
		Metadata:  MetadataView{},
		Resources: NewResourceRegistry(),
		Tasks:     NewTaskRegistry(context.Background(), nil),
	}

	if err := dependencies.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestRuntimeDependenciesConsumersRemainOptionalAndAccessible(t *testing.T) {
	dependencies := RuntimeDependencies{
		Config:    &config.EffectiveConfig{},
		Secrets:   testGenerationCapability(t),
		Resources: NewResourceRegistry(),
		Tasks:     NewTaskRegistry(context.Background(), nil),
	}
	if err := dependencies.Validate(); err != nil {
		t.Fatalf("Validate() without Consumers error = %v", err)
	}

	bindings, err := NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	dependencies.Consumers = bindings
	if err := dependencies.Validate(); err != nil {
		t.Fatalf("Validate() with Consumers error = %v", err)
	}
	if dependencies.Consumers != bindings {
		t.Fatal("RuntimeDependencies did not retain Consumers")
	}
}
