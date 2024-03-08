package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/server"
)

var (
	cfgFile string
	addr    string
)

func init() {
	// cobra.OnInitialize(initConfig)
	rootCmd.Flags().StringVarP(&cfgFile, "config", "c", "", "config file (default is config.yml;required)")
	rootCmd.PersistentFlags().Bool("viper", true, "Use Viper for configuration")
	rootCmd.PersistentFlags().StringVar(&addr, "addr", "", "addr like 0.0.0.0:9100")

	rootCmd.MarkFlagRequired("config")
	viper.SetDefault("author", "blueking-paas")

	viper.AutomaticEnv()
	viper.SetEnvPrefix("APIGW")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
}

var rootCmd = &cobra.Command{
	Use:   "apisix",
	Short: "an golang version of apisix",
	// Long: `A Fast and Flexible Static Site Generator built with love by spf13 and friends in Go.
	// 			  Complete documentation is available at http://hugo.spf13.com`,
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
		if addr != "" {
			logger.Infof("Get addr from command line: %s", addr)
			viper.SetDefault("server.addr", addr)
		}
	}
	// initConfig()

	// if globalConfig.Debug {
	// 	fmt.Println(viper.AllSettings())
	// 	fmt.Println(globalConfig)
	// }

	// 2. watch the signal
	ctx, cancelFunc := context.WithCancel(context.Background())
	go func() {
		interrupt(cancelFunc)
	}()

	// 3. new and start server
	server, err := server.NewServer()
	if err != nil {
		panic(err)
	}
	server.Start(ctx)
}

// a context canceled when SIGINT or SIGTERM are notified
func interrupt(onSignal func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)

	for s := range c {
		logger.Infof("Caught signal %s. Exiting.", s)
		onSignal()
		close(c)
	}
}
