package authz_casbin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCasbinFileDependencyFailureIsRetryable(t *testing.T) {
	directory := t.TempDir()
	modelPath := filepath.Join(directory, "model.conf")
	policyPath := filepath.Join(directory, "policy.csv")
	p := &Plugin{config: Config{
		ModelPath: modelPath, PolicyPath: policyPath, Username: "X-User",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() accepted unavailable Casbin files")
	}
	if p.enforcer != nil {
		t.Fatal("failed Casbin load retained a partial enforcer")
	}

	if err := os.WriteFile(modelPath, []byte("malformed model"), 0o600); err != nil {
		t.Fatalf("write malformed model: %v", err)
	}
	if err := os.WriteFile(policyPath, []byte(testPolicy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() accepted malformed Casbin model")
	}
	if p.enforcer != nil {
		t.Fatal("malformed Casbin load retained a partial enforcer")
	}

	if err := os.WriteFile(modelPath, []byte(testModel), 0o600); err != nil {
		t.Fatalf("write valid model: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() retry error = %v", err)
	}
	if p.enforcer == nil {
		t.Fatal("successful Casbin retry did not install an enforcer")
	}
}
