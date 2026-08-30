package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
)

func loadEffectiveForCommand(configPath string) (*config.EffectiveConfig, error) {
	_, effective, _, err := loadEffectiveForStartup(configPath)
	return effective, err
}

func loadEffectiveForStartup(configPath string) (
	*capability.Manifest, *config.EffectiveConfig, *capability.SecretDeclarationCatalog, error,
) {
	manifest, err := capability.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load capability manifest: %w", err)
	}
	effective, err := loadEffectiveForManifest(configPath, manifest)
	if err != nil {
		return nil, nil, nil, err
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build secret declaration catalog: %w", err)
	}
	return manifest, effective, catalog, nil
}

func loadEffectiveForManifest(
	configPath string,
	_ *capability.Manifest,
) (*config.EffectiveConfig, error) {
	paths, err := config.DefaultRuntimePaths()
	if err != nil {
		return nil, fmt.Errorf("resolve default runtime paths: %w", err)
	}
	defaultPath, err := filepath.Abs(config.DefaultConfigFile)
	if err != nil {
		return nil, fmt.Errorf("resolve default config path: %w", err)
	}
	overridePath := ""
	if configPath != "" {
		overridePath, err = filepath.Abs(configPath)
		if err != nil {
			return nil, fmt.Errorf("resolve override config path: %w", err)
		}
		if filepath.Clean(overridePath) == filepath.Clean(defaultPath) {
			overridePath = ""
		}
	}
	return config.LoadEffective(config.LoadRequest{
		DefaultPath:  defaultPath,
		OverridePath: overridePath,
		DefaultPaths: paths,
		Environment:  environmentMap(os.Environ()),
	})
}

func newConfigCommand(load func(string) (*config.EffectiveConfig, error)) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "inspect effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	command.AddCommand(newConfigTestCommand(load))
	return command
}

func newConfigTestCommand(load func(string) (*config.EffectiveConfig, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "validate configuration without starting the server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := loadCommandEffective(cmd, load); err != nil {
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "configuration is valid")
			return err
		},
	}
}

func loadCommandEffective(
	cmd *cobra.Command,
	load func(string) (*config.EffectiveConfig, error),
) (*config.EffectiveConfig, error) {
	configPath, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	return load(configPath)
}
