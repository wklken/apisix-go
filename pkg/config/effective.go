package config

type EffectiveConfig struct {
	Config Config
	Paths  RuntimePaths
}

type LoadRequest struct {
	DefaultPath  string
	OverridePath string
	DefaultPaths RuntimePaths
	Environment  map[string]string
}
