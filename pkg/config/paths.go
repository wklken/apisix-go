package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type RuntimePaths struct {
	RuntimeDir string `mapstructure:"runtime_dir" json:"runtime_dir"`
	LogDir     string `mapstructure:"log_dir" json:"log_dir"`
	TempDir    string `mapstructure:"temp_dir" json:"temp_dir"`
}

func DefaultRuntimePaths() (RuntimePaths, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return RuntimePaths{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	tempDir := os.TempDir()

	paths := RuntimePaths{
		RuntimeDir: filepath.Join(cacheDir, "apisix-go", "run"),
		LogDir:     filepath.Join(cacheDir, "apisix-go", "log"),
		TempDir:    filepath.Join(tempDir, "apisix-go"),
	}
	for name, path := range map[string]string{
		"runtime_dir": paths.RuntimeDir,
		"log_dir":     paths.LogDir, "temp_dir": paths.TempDir,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return RuntimePaths{}, fmt.Errorf("default %s is not absolute", name)
		}
	}
	return paths, nil
}
