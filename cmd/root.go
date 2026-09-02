package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/server"

	_ "github.com/wklken/apisix-go/pkg/observability/otel"
	_ "github.com/wklken/apisix-go/pkg/proxy"
)

type rootOptions struct {
	configPath string
}

type reloadFunc func() error

type serverLifecycle interface {
	Start(context.Context) error
	Shutdown(context.Context) error
}

type startupFactories struct {
	loadEffective   func(string) (*config.EffectiveConfig, error)
	newCatalog      func() (*capability.SecretDeclarationCatalog, error)
	newEncryption   func(*config.EffectiveConfig, *capability.SecretDeclarationCatalog) data_encryption.Service
	configureLogger func(*config.Config) error
	newResolver     func(data_encryption.Service) (*secret.GenerationSecretResolver, error)
	closeResolver   func(*secret.GenerationSecretResolver, context.Context) error
	newServer       func(
		*config.EffectiveConfig,
		data_encryption.Service,
		*secret.GenerationSecretResolver,
	) (serverLifecycle, error)
	runServer func(serverLifecycle, reloadFunc) error
}

func defaultStartupFactories() startupFactories {
	return startupFactories{
		loadEffective: loadEffective,
		newCatalog:    capability.NewSecretDeclarationCatalog,
		newEncryption: func(
			effective *config.EffectiveConfig,
			catalog *capability.SecretDeclarationCatalog,
		) data_encryption.Service {
			return data_encryption.NewService(
				effective.Config.Apisix.DataEncryption.EnableEncryptFields,
				effective.Config.Apisix.DataEncryption.Keyring,
				catalog,
			)
		},
		configureLogger: configureLogger,
		newResolver:     secret.NewGenerationSecretResolver,
		closeResolver: func(resolver *secret.GenerationSecretResolver, ctx context.Context) error {
			return resolver.Close(ctx)
		},
		newServer: func(
			effective *config.EffectiveConfig,
			encryption data_encryption.Service,
			resolver *secret.GenerationSecretResolver,
		) (serverLifecycle, error) {
			return server.NewServer(effective, encryption, resolver)
		},
		runServer: runServer,
	}
}

func (factories startupFactories) withDefaults() startupFactories {
	defaults := defaultStartupFactories()
	if factories.loadEffective == nil {
		factories.loadEffective = defaults.loadEffective
	}
	if factories.newCatalog == nil {
		factories.newCatalog = defaults.newCatalog
	}
	if factories.newEncryption == nil {
		factories.newEncryption = defaults.newEncryption
	}
	if factories.configureLogger == nil {
		factories.configureLogger = defaults.configureLogger
	}
	if factories.newResolver == nil {
		factories.newResolver = defaults.newResolver
	}
	if factories.closeResolver == nil {
		factories.closeResolver = defaults.closeResolver
	}
	if factories.newServer == nil {
		factories.newServer = defaults.newServer
	}
	if factories.runServer == nil {
		factories.runServer = defaults.runServer
	}
	return factories
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
		Short:         "APISIX 3.17 HTTP data plane release candidate; stream subsystem excluded",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return startWithOptions(*options)
		},
	}
	root.PersistentFlags().StringVarP(&options.configPath, "config", "c", config.DefaultConfigFile, "config file")
	root.AddCommand(newVersionCommand())
	root.AddCommand(newConfigCommand(loadEffectiveForCommand))
	return root
}

func startWithOptions(options rootOptions) error {
	return startWithOptionsWithFactories(options, defaultStartupFactories())
}

func startWithOptionsWithFactories(options rootOptions, factories startupFactories) error {
	factories = factories.withDefaults()
	effective, err := factories.loadEffective(options.configPath)
	if err != nil {
		return fmt.Errorf("load effective config: %w", err)
	}
	catalog, err := factories.newCatalog()
	if err != nil {
		return fmt.Errorf("build secret declaration catalog: %w", err)
	}
	encryption := factories.newEncryption(effective, catalog)
	if err := factories.configureLogger(&effective.Config); err != nil {
		return fmt.Errorf("configure logger: %s", err)
	}
	logger.Infof("startup config summary: %v", config.CapabilitySummary(&effective.Config))
	resolver, err := factories.newResolver(encryption)
	if err != nil {
		return fmt.Errorf("create generation secret resolver: %w", err)
	}
	logger.Info("Starting server")
	srv, err := factories.newServer(effective, encryption, resolver)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	reload := newLoggerReloader(
		options.configPath,
		effective.Config,
		factories.loadEffective,
		factories.configureLogger,
	)
	return factories.runServer(srv, reload)
}

func newLoggerReloader(
	configPath string,
	current config.Config,
	load func(string) (*config.EffectiveConfig, error),
	configure func(*config.Config) error,
) reloadFunc {
	return func() error {
		effective, err := load(configPath)
		if err != nil {
			return fmt.Errorf("reload effective config: %w", err)
		}
		next := effective.Config
		currentStatic := current
		nextStatic := next
		currentStatic.NginxConfig.ErrorLogLevel = ""
		nextStatic.NginxConfig.ErrorLogLevel = ""
		if !reflect.DeepEqual(currentStatic, nextStatic) {
			return errors.New("SIGHUP reload changed unsupported static configuration")
		}
		if current.NginxConfig.ErrorLogLevel == next.NginxConfig.ErrorLogLevel {
			return nil
		}
		if err := configure(&next); err != nil {
			return fmt.Errorf("configure logger: %w", err)
		}
		current = next
		return nil
	}
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

// runServer owns process signals. SIGHUP performs the bounded reload callback;
// termination signals trigger graceful shutdown. main remains the only
// process-exit boundary.
func runServer(srv serverLifecycle, reload reloadFunc) error {
	reloadSignals := make(chan os.Signal, 1)
	terminationSignals := make(chan os.Signal, 1)
	signal.Notify(reloadSignals, syscall.SIGHUP)
	signal.Notify(terminationSignals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer signal.Stop(reloadSignals)
	defer signal.Stop(terminationSignals)
	return runServerWithSignals(srv, reloadSignals, terminationSignals, reload)
}

func runServerWithSignals(
	srv serverLifecycle,
	reloadSignals <-chan os.Signal,
	terminationSignals <-chan os.Signal,
	reload reloadFunc,
) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Start(ctx)
	}()
	reloadDone := make(chan error, 1)
	reloading := false
	reloadPending := false
	startReload := func() {
		reloading = true
		go func() {
			reloadDone <- reload()
		}()
	}
	drainReloadSignals := func() {
		for {
			select {
			case <-reloadSignals:
			default:
				return
			}
		}
	}
	shutdown := func(received os.Signal) error {
		logger.Infof("received signal %s, shutting down", received)
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		shutdownErr := srv.Shutdown(shutdownCtx)
		shutdownCancel()
		cancel()
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		return nil
	}

	for {
		select {
		case received := <-terminationSignals:
			return shutdown(received)
		default:
		}
		select {
		case err := <-serveErr:
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("server stopped: %w", err)
		case <-reloadSignals:
			if reload == nil {
				logger.Error("SIGHUP reload callback is not configured")
				continue
			}
			if reloading {
				reloadPending = true
				continue
			}
			startReload()
		case err := <-reloadDone:
			reloading = false
			if err != nil {
				logger.Errorf("SIGHUP reload rejected: %v", err)
			} else {
				logger.Info("SIGHUP configuration reload completed")
			}
			if reloadPending {
				reloadPending = false
				drainReloadSignals()
				startReload()
			}
		case received := <-terminationSignals:
			return shutdown(received)
		}
	}
}
