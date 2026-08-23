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

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/server"

	_ "github.com/wklken/apisix-go/pkg/observability/otel"
	_ "github.com/wklken/apisix-go/pkg/proxy"
)

var errSIGHUPReloadUnsupported = errors.New("SIGHUP reload is unsupported")

type rootOptions struct {
	configPath string
	setValues  []string
}

type serverLifecycle interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

func configureLogger(cfg *config.Config) error {
	if cfg == nil {
		return logger.ConfigureLevel("")
	}
	return logger.ConfigureLevel(cfg.NginxConfig.ErrorLogLevel)
}

func Execute() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func Start() error {
	return startWithOptions(rootOptions{configPath: config.DefaultConfigFile})
}

func newRootCommand() *cobra.Command {
	options := &rootOptions{configPath: config.DefaultConfigFile}
	root := &cobra.Command{
		Use:           "apisix",
		Short:         "an golang version of apisix, not production ready",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return startWithOptions(*options)
		},
	}
	root.PersistentFlags().StringVarP(&options.configPath, "config", "c", config.DefaultConfigFile, "config file")
	root.PersistentFlags().StringArrayVar(&options.setValues, "set", nil, "set a static configuration path to a value")
	root.AddCommand(newVersionCommand())
	root.AddCommand(newConfigCommand(loadEffectiveForCommand))
	return root
}

func startWithOptions(options rootOptions) error {
	effective, err := loadEffectiveForCommand(options.configPath, options.setValues)
	if err != nil {
		return fmt.Errorf("load effective config: %w", err)
	}
	encryption := data_encryption.NewService(
		effective.Config.Apisix.DataEncryption.EnableEncryptFields,
		effective.Config.Apisix.DataEncryption.Keyring,
	)
	if err := configureLogger(&effective.Config); err != nil {
		return fmt.Errorf("configure logger: %s", err)
	}

	logger.Infof("startup config summary: %v", config.CapabilitySummary(&effective.Config))

	logger.Info("Starting server")
	srv, err := server.NewServer(effective, encryption)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return runServer(srv)
}

func environmentMap(entries []string) map[string]string {
	environment := make(map[string]string, len(entries))
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		environment[name] = value
	}
	return environment
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
