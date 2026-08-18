package config

import (
	"os/exec"
	"strings"
	"testing"
)

func TestConfigPackageDoesNotDependOnIngressControllerOrKubernetes(t *testing.T) {
	command := exec.Command("go", "list", "-mod=readonly", "-deps", ".")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, output)
	}

	for dependency := range strings.FieldsSeq(string(output)) {
		if dependency == "github.com/apache/apisix-ingress-controller" ||
			strings.HasPrefix(dependency, "github.com/apache/apisix-ingress-controller/") ||
			dependency == "k8s.io" || strings.HasPrefix(dependency, "k8s.io/") {
			t.Fatalf("pkg/config unexpectedly depends on %q", dependency)
		}
	}
}
