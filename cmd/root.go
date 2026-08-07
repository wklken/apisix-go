package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/server"

	_ "github.com/wklken/apisix-go/pkg/observability/otel"
	_ "github.com/wklken/apisix-go/pkg/proxy"
)

var cfgFile string

var globalConfig *config.Config

func initConfig() {
	var err error
	globalConfig, err = config.Load()
	if err != nil {
		logger.Fatalf("could not load configurations from file, %s", err)
	}
}

func configureLogger(cfg *config.Config) error {
	if cfg == nil {
		return logger.ConfigureLevel("")
	}
	return logger.ConfigureLevel(cfg.NginxConfig.ErrorLogLevel)
}

func init() {
	rootCmd.Flags().StringVarP(&cfgFile, "config", "c", "conf/config-default.yaml", "config file")
	rootCmd.PersistentFlags().Bool("viper", true, "Use Viper for configuration")

	viper.SetDefault("author", "wklken")

	viper.AutomaticEnv()
	viper.SetEnvPrefix("APISIXGO")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
}

var rootCmd = &cobra.Command{
	Use:   "apisix",
	Short: "an golang version of apisix, not production ready",
	Run: func(cmd *cobra.Command, args []string) {
		Start()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func startupConfigSummary(cfg *config.Config) map[string]any {
	if cfg == nil {
		return nil
	}
	configProvider := strings.ToLower(strings.TrimSpace(cfg.Deployment.RoleTraditional.ConfigProvider))
	if configProvider == "" {
		configProvider = strings.ToLower(strings.TrimSpace(cfg.Deployment.RoleDataPlane.ConfigProvider))
	}
	if configProvider == "" {
		configProvider = strings.ToLower(strings.TrimSpace(cfg.Deployment.RoleControlPlane.ConfigProvider))
	}
	return map[string]any{
		"debug":             cfg.Debug,
		"role":              cfg.Deployment.Role,
		"config_provider":   configProvider,
		"node_listen":       cfg.Apisix.ListenAddresses(),
		"enabled_plugins":   len(cfg.Plugins),
		"etcd_endpoints":    len(cfg.Deployment.Etcd.Host),
		"enabled_ssl":       cfg.Apisix.Ssl.Enable,
		"admin_api_version": cfg.Deployment.Admin.AdminAPIVersion,
	}
}

func Start() {
	fmt.Println("It's apisix")

	// FIXME: merge config.yaml and config-default.yaml
	// load global config
	if cfgFile != "" {
		// Use config file from the flag.
		// log.Infof("Load config file: %s", cfgFile)
		viper.SetConfigFile(cfgFile)
	}
	initConfig()
	if err := configureLogger(globalConfig); err != nil {
		logger.Fatalf("configure logger: %s", err)
	}

	logger.Infof("startup config summary: %v", startupConfigSummary(globalConfig))

	logger.Info("Starting server")
	server, err := server.NewServer()
	if err != nil {
		panic(err)
	}
	server.Start()
}
