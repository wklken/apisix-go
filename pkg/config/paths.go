package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type RuntimePaths struct {
	DataDir    string `mapstructure:"data_dir" json:"data_dir"`
	RuntimeDir string `mapstructure:"runtime_dir" json:"runtime_dir"`
	LogDir     string `mapstructure:"log_dir" json:"log_dir"`
	TempDir    string `mapstructure:"temp_dir" json:"temp_dir"`
}

func DefaultRuntimePaths() (RuntimePaths, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	tempDir := os.TempDir()

	paths := RuntimePaths{
		DataDir:    filepath.Join(configDir, "apisix-go", "data"),
		RuntimeDir: filepath.Join(cacheDir, "apisix-go", "run"),
		LogDir:     filepath.Join(cacheDir, "apisix-go", "log"),
		TempDir:    filepath.Join(tempDir, "apisix-go"),
	}
	for name, path := range map[string]string{
		"data_dir": paths.DataDir, "runtime_dir": paths.RuntimeDir,
		"log_dir": paths.LogDir, "temp_dir": paths.TempDir,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return RuntimePaths{}, fmt.Errorf("default %s is not absolute", name)
		}
	}
	return paths, nil
}

func JournalPath(effective *EffectiveConfig) string {
	if effective == nil || effective.Paths.DataDir == "" || !filepath.IsAbs(effective.Paths.DataDir) {
		return ""
	}
	return filepath.Join(effective.Paths.DataDir, "apisix-go-store.db")
}
