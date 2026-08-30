package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/server"
	"github.com/wklken/apisix-go/pkg/store"

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

type startupFactories struct {
	loadManifest    func() (*capability.Manifest, error)
	loadEffective   func(string, []string, *capability.Manifest) (*config.EffectiveConfig, error)
	newCatalog      func(*capability.Manifest) (*capability.SecretDeclarationCatalog, error)
	newEncryption   func(*config.EffectiveConfig, *capability.SecretDeclarationCatalog) data_encryption.Service
	configureLogger func(*config.Config) error
	newResolver     func(data_encryption.Service) (*secret.GenerationSecretResolver, error)
	closeResolver   func(*secret.GenerationSecretResolver, context.Context) error
	mkdirAll        func(string, os.FileMode) error
	openJournal     func(string) (generation.Journal, error)
	newServer       func(
		*config.EffectiveConfig,
		*capability.Manifest,
		data_encryption.Service,
		*secret.GenerationSecretResolver,
		generation.Journal,
		generation.RecoveryState,
	) (serverLifecycle, error)
	runServer func(serverLifecycle) error
}

func defaultStartupFactories() startupFactories {
	return startupFactories{
		loadManifest:  capability.Load,
		loadEffective: loadEffectiveForManifest,
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
		mkdirAll: os.MkdirAll,
		openJournal: func(path string) (generation.Journal, error) {
			return store.OpenJournal(path)
		},
		newServer: func(
			effective *config.EffectiveConfig,
			manifest *capability.Manifest,
			encryption data_encryption.Service,
			resolver *secret.GenerationSecretResolver,
			journal generation.Journal,
			recovery generation.RecoveryState,
		) (serverLifecycle, error) {
			return server.NewServer(effective, manifest, encryption, resolver, journal, recovery)
		},
		runServer: runServer,
	}
}

func (factories startupFactories) withDefaults() startupFactories {
	defaults := defaultStartupFactories()
	if factories.loadManifest == nil {
		factories.loadManifest = defaults.loadManifest
	}
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
	if factories.mkdirAll == nil {
		factories.mkdirAll = defaults.mkdirAll
	}
	if factories.openJournal == nil {
		factories.openJournal = defaults.openJournal
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
	return startWithOptionsWithFactories(options, defaultStartupFactories())
}

func startWithOptionsWithFactories(options rootOptions, factories startupFactories) error {
	factories = factories.withDefaults()
	manifest, err := factories.loadManifest()
	if err != nil {
		return fmt.Errorf("load capability manifest: %w", err)
	}
	effective, err := factories.loadEffective(options.configPath, options.setValues, manifest)
	if err != nil {
		return fmt.Errorf("load effective config: %w", err)
	}
	catalog, err := factories.newCatalog(manifest)
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
	journalPath := config.JournalPath(effective)
	if journalPath == "" || !filepath.IsAbs(journalPath) {
		return errors.Join(
			errors.New("generation journal path is invalid"),
			factories.closeResolver(resolver, context.Background()),
		)
	}
	if err := factories.mkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		return errors.Join(
			fmt.Errorf("create generation data directory: %w", err),
			factories.closeResolver(resolver, context.Background()),
		)
	}
	journal, err := factories.openJournal(journalPath)
	if err != nil {
		return errors.Join(
			fmt.Errorf("open generation journal: %w", err),
			factories.closeResolver(resolver, context.Background()),
		)
	}
	recovery, err := journal.Recover(context.Background())
	if err != nil {
		return errors.Join(
			fmt.Errorf("recover generation journal: %w", err),
			journal.Close(),
			factories.closeResolver(resolver, context.Background()),
		)
	}

	logger.Info("Starting server")
	srv, err := factories.newServer(effective, manifest, encryption, resolver, journal, recovery)
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}
	return factories.runServer(srv)
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
func runServer(srv serverLifecycle) error {
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
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
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
