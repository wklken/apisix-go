package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
)

func parseSetOverrides(values []string) (map[string]any, error) {
	result := make(map[string]any, len(values))
	for _, raw := range values {
		path, value, ok := strings.Cut(raw, "=")
		if !ok || path == "" {
			return nil, errors.New("--set must use path=value")
		}
		if err := config.ValidateStaticOverridePath(path); err != nil {
			if path == "deployment.profile" {
				return nil, err
			}
			return nil, errors.New("--set path does not map to a static configuration field")
		}
		if _, exists := result[path]; exists {
			return nil, errors.New("--set path is repeated")
		}
		result[path] = value
	}
	return result, nil
}

func loadEffectiveForCommand(configPath string, setValues []string) (*config.EffectiveConfig, error) {
	_, effective, _, err := loadEffectiveForStartup(configPath, setValues)
	return effective, err
}

func loadEffectiveForStartup(
	configPath string,
	setValues []string,
) (*capability.Manifest, *config.EffectiveConfig, *capability.SecretDeclarationCatalog, error) {
	manifest, err := capability.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load capability manifest: %w", err)
	}
	effective, err := loadEffectiveForManifest(configPath, setValues, manifest)
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
	setValues []string,
	manifest *capability.Manifest,
) (*config.EffectiveConfig, error) {
	overrides, err := parseSetOverrides(setValues)
	if err != nil {
		return nil, err
	}
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
		CLIOverrides: overrides,
		Manifest:     manifest,
	})
}

func newConfigCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command {
	command := &cobra.Command{
		Use:   "config",
		Short: "inspect effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	command.AddCommand(newConfigTestCommand(load), newConfigDumpCommand(load))
	return command
}

func newConfigTestCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command {
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

func newConfigDumpCommand(load func(string, []string) (*config.EffectiveConfig, error)) *cobra.Command {
	var effectiveFlag, redactedFlag bool
	command := &cobra.Command{
		Use:   "dump",
		Short: "render the effective configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !effectiveFlag || !redactedFlag {
				return errors.New("config dump requires --effective and --redacted")
			}
			effective, err := loadCommandEffective(cmd, load)
			if err != nil {
				return err
			}
			data, err := config.RenderEffectiveRedacted(effective)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	command.Flags().BoolVar(&effectiveFlag, "effective", false, "confirm effective configuration output")
	command.Flags().BoolVar(&redactedFlag, "redacted", false, "confirm redacted configuration output")
	return command
}

func loadCommandEffective(
	cmd *cobra.Command,
	load func(string, []string) (*config.EffectiveConfig, error),
) (*config.EffectiveConfig, error) {
	configPath, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	setValues, err := cmd.Flags().GetStringArray("set")
	if err != nil {
		return nil, err
	}
	return load(configPath, setValues)
}
