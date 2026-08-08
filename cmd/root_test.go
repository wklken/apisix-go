package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
)

func TestStartupConfigSummaryExcludesSecrets(t *testing.T) {
	cfg := &config.Config{}
	cfg.Deployment.Admin.AdminKey = []config.AdminKey{{Key: "admin-secret"}}
	cfg.Deployment.Etcd.Password = "etcd-secret"
	cfg.Apisix.DataEncryption.Keyring = []string{"0123456789abcdef"}

	encoded, err := json.Marshal(startupConfigSummary(cfg))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	for _, secret := range []string{"admin-secret", "etcd-secret", "0123456789abcdef"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("startup summary contains %q: %s", secret, encoded)
		}
	}
}

func TestConfigureLoggerUsesLoadedErrorLogLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	cfg := &config.Config{NginxConfig: config.NginxConfig{ErrorLogLevel: "debug"}}
	if err := configureLogger(cfg); err != nil {
		t.Fatalf("configureLogger() error = %v", err)
	}
	if !logger.DebugEnabled() {
		t.Fatal("debug logging is disabled after debug configuration")
	}
}

func TestConfigureLoggerAcceptsNilAndRejectsInvalidLevel(t *testing.T) {
	t.Cleanup(func() { _ = logger.ConfigureLevel("info") })
	if err := configureLogger(nil); err != nil {
		t.Fatalf("configureLogger(nil) error = %v", err)
	}
	cfg := &config.Config{NginxConfig: config.NginxConfig{ErrorLogLevel: "bogus"}}
	if err := configureLogger(cfg); err == nil {
		t.Fatal("configureLogger(invalid level) error = nil")
	}
}

func TestRootCommandConfigFlagMetadata(t *testing.T) {
	flag := rootCmd.Flags().Lookup("config")
	if flag == nil {
		t.Fatal("config flag is not registered")
	}
	if flag.Shorthand != "c" {
		t.Fatalf("config shorthand = %q, want c", flag.Shorthand)
	}
	if flag.DefValue != "conf/config-default.yaml" {
		t.Fatalf("config default = %q, want conf/config-default.yaml", flag.DefValue)
	}
}
