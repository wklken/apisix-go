package config

type EffectiveConfig struct {
	Config Config
}

type LoadRequest struct {
	DefaultPath  string
	OverridePath string
	Environment  map[string]string
}
