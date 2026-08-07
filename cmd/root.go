package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/server"

	_ "github.com/wklken/apisix-go/pkg/observability/otel"
	_ "github.com/wklken/apisix-go/pkg/proxy"
)

var cfgFile string

var globalConfig *config.Config

func initConfig() error {
	var err error
	globalConfig, err = config.Load()
	if err != nil {
		return fmt.Errorf("load configurations from file: %w", err)
	}
	return nil
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
	RunE: func(cmd *cobra.Command, args []string) error {
		return Start()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func Start() error {
	fmt.Println("It's apisix")

	// FIXME: merge config.yaml and config-default.yaml
	// load global config
	if cfgFile != "" {
		// Use config file from the flag.
		// log.Infof("Load config file: %s", cfgFile)
		viper.SetConfigFile(cfgFile)
	}
	if err := initConfig(); err != nil {
		return err
	}
	if err := configureLogger(globalConfig); err != nil {
		return fmt.Errorf("configure logger: %s", err)
	}

	fmt.Printf("global config: %+v\n", globalConfig)
	b, _ := json.Marshal(globalConfig)
	fmt.Println("global config json:", string(b))

	if globalConfig.Debug {
		fmt.Println(viper.AllSettings())
		fmt.Println(globalConfig)
	}

	logger.Info("Starting server")
	srv, err := server.NewServer()
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return runServer(srv)
}

// runServer owns the process shutdown path: a signal triggers a graceful
// shutdown, and a serving error cancels the root context and enters the
// normal shutdown path. main remains the only process-exit boundary.
func runServer(srv *server.Server) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Start(ctx)
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(signals)

	select {
	case err := <-serveErr:
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server stopped: %w", err)
	case received := <-signals:
		logger.Infof("received signal %s, shutting down", received)
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 30*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		cancel()
		return nil
	}
}
