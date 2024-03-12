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

	_ "github.com/wklken/apisix-go/pkg/observability/metrics"
	_ "github.com/wklken/apisix-go/pkg/observability/otel"
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
	// cobra.OnInitialize(initConfig)
	rootCmd.Flags().StringVarP(&cfgFile, "config", "c", "conf/config-default.yaml", "config file")
	rootCmd.PersistentFlags().Bool("viper", true, "Use Viper for configuration")
	// rootCmd.PersistentFlags().StringVar(&addr, "addr", "", "addr like 0.0.0.0:9100")

	// rootCmd.MarkFlagRequired("config")
	viper.SetDefault("author", "wklken")

	viper.AutomaticEnv()
	viper.SetEnvPrefix("APISIXGO")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
}

var rootCmd = &cobra.Command{
	Use:   "apisix",
	Short: "an golang version of apisix",
	Run: func(cmd *cobra.Command, args []string) {
		// Do Stuff Here
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

	// 1. do init first
	// load global config
	if cfgFile != "" {
		// Use config file from the flag.
		// log.Infof("Load config file: %s", cfgFile)
		viper.SetConfigFile(cfgFile)

		// if addr from command line args
		// if addr != "" {
		// 	logger.Infof("Get addr from command line: %s", addr)
		// 	viper.SetDefault("server.addr", addr)
		// }
	}
	initConfig()

	if globalConfig.Debug {
		fmt.Println(viper.AllSettings())
		fmt.Println(globalConfig)
	}

	// 3. new and start server
	logger.Info("Starting server")
	server, err := server.NewServer()
	if err != nil {
		panic(err)
	}
	server.Start()
}
