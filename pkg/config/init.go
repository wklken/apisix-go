package config

import (
	"fmt"

	"github.com/spf13/viper"
)

var GlobalConfig *Config

func Load() (*Config, error) {
	// viper.SetConfigName("config")
	// viper.AddConfigPath(".")

	err := viper.ReadInConfig()
	if err != nil {
		return nil, fmt.Errorf("fail to load config file, %w", err)
	}

	var cfg Config
	err = viper.Unmarshal(&cfg)
	if err != nil {
		return nil, err
	}

	GlobalConfig = &cfg

	return &cfg, nil
}
