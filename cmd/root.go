package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/server"

	_ "github.com/wklken/apisix-go/pkg/observability/otel"
	_ "github.com/wklken/apisix-go/pkg/proxy"
)

var cfgFile string

var globalConfig *config.Config

var errSIGHUPReloadUnsupported = errors.New("SIGHUP reload is unsupported")

type serverLifecycle interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

func initConfig() error {
	var err error
	globalConfig, err = config.Load(cfgFile)
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
}

var rootCmd = &cobra.Command{
	Use:   "apisix",
	Short: "an golang version of apisix, not production ready",
	Args:  cobra.NoArgs,
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
	if err := initConfig(); err != nil {
		return err
	}
	if err := configureLogger(globalConfig); err != nil {
		return fmt.Errorf("configure logger: %s", err)
	}

	logger.Infof("startup config summary: %v", config.CapabilitySummary(globalConfig))

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
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(signals)
	return runServerWithSignals(srv, signals)
}

func runServerWithSignals(srv serverLifecycle, signals <-chan os.Signal) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Start(ctx)
	}()

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
		shutdownErr := srv.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		if received == syscall.SIGHUP {
			return errSIGHUPReloadUnsupported
		}
		return nil
	}
}
