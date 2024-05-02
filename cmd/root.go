package cmd

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

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

func Start() {
	fmt.Println("It's apisix")

	// random seed init here
	rand.Seed(time.Now().UnixNano())

	// FIXME: merge config.yaml and config-default.yaml
	// load global config
	if cfgFile != "" {
		// Use config file from the flag.
		// log.Infof("Load config file: %s", cfgFile)
		viper.SetConfigFile(cfgFile)
	}
	initConfig()

	if globalConfig.Debug {
		fmt.Println(viper.AllSettings())
		fmt.Println(globalConfig)
	}

	logger.Info("Starting server")
	server, err := server.NewServer()
	if err != nil {
		panic(err)
	}
	server.Start()
}
