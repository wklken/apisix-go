package pluginintegration

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestDifferentialSuiteHasDurableEvidenceForEveryFactory(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatalf("load capability manifest: %v", err)
	}
	repoRoot, err := repositoryRootFromWorkingDirectory()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := loadDifferentialCatalog(differentialCatalogPath(repoRoot))
	if err != nil {
		t.Fatal(err)
	}

	var problems []string
	factoryCount := 0
	for _, pluginName := range catalog.RequiredPlugins {
		plugin, found := manifest.Plugin(pluginName)
		if !found {
			problems = append(problems, "missing capability "+pluginName)
			continue
		}
		if plugin.Evidence.Unit.State != capability.EvidenceVerified {
			problems = append(problems, fmt.Sprintf("%s unit=%s", pluginName, plugin.Evidence.Unit.State))
		}
		if plugin.Evidence.Upstream.State != capability.EvidenceVerified &&
			plugin.Evidence.Upstream.State != capability.EvidenceNotApplicable {
			problems = append(
				problems,
				fmt.Sprintf("%s converted_upstream=%s", pluginName, plugin.Evidence.Upstream.State),
			)
		}
		factoryCount += len(plugin.Factories)
		if len(plugin.Factories) <= 1 {
			continue
		}
		aliasEvidence := false
		for _, ref := range plugin.Evidence.Upstream.Refs {
			if strings.HasPrefix(ref, "pkg/plugin/init_test.go#TestNewPreservesHistoricalFactoryAliases") {
				aliasEvidence = true
				break
			}
		}
		if !aliasEvidence {
			problems = append(problems, pluginName+" has multiple factories without direct alias evidence")
		}
	}

	sort.Strings(problems)
	if len(problems) != 0 {
		t.Fatalf("all-plugin durable evidence problems = %v", problems)
	}
	t.Logf(
		"compatibility catalog has durable evidence for %d capabilities and %d factory keys",
		len(catalog.RequiredPlugins),
		factoryCount,
	)
}
