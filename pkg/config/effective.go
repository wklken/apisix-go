package config

import "github.com/wklken/apisix-go/pkg/capability"

type SourceKind string

const (
	SourceBuiltin      SourceKind = "builtin"
	SourceDefaultFile  SourceKind = "default_file"
	SourceOverrideFile SourceKind = "override_file"
	SourceAPISIXEnv    SourceKind = "apisix_env"
	SourceAPISIXGOEnv  SourceKind = "apisixgo_env"
	SourceCLI          SourceKind = "cli"
)

type FieldSource struct {
	Kind     SourceKind `json:"kind"`
	Origin   string     `json:"origin"`
	Explicit bool       `json:"explicit"`
}

type Provenance map[string]FieldSource

type EffectiveConfig struct {
	Config     Config
	Provenance Provenance
	Profiles   ProfileSelection
	Paths      RuntimePaths
}

type LoadRequest struct {
	DefaultPath  string
	OverridePath string
	DefaultPaths RuntimePaths
	Environment  map[string]string
	CLIOverrides map[string]any
	Manifest     *capability.Manifest
}
