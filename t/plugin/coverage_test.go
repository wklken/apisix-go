package pluginintegration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	documentedPluginName   = regexp.MustCompile("`([^`]+)`")
	upstreamSourceAbsences = map[string]string{
		"GM":              "no Apache APISIX t/plugin source at the pinned commit",
		"proxy-buffering": "no Apache APISIX t/plugin source at the pinned commit",
	}
)

func TestSupportedPluginManifestSelection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "plugins.md"))
	if err != nil {
		t.Fatalf("read docs/plugins.md: %v", err)
	}

	plugins, err := supportedPluginNames(data)
	if err != nil {
		t.Fatalf("supportedPluginNames() error = %v", err)
	}
	if got := len(plugins); got != 100 {
		t.Fatalf("supported plugins = %d, want 100", got)
	}

	manifests := make(map[string]bool, len(plugins)-len(upstreamSourceAbsences))
	for _, pluginName := range plugins {
		if _, absent := upstreamSourceAbsences[pluginName]; !absent {
			manifests[pluginName] = true
		}
	}
	if problems := manifestCoverageProblems(plugins, manifests); len(problems) != 0 {
		t.Fatalf("complete manifest set problems = %v", problems)
	}

	delete(manifests, "redirect")
	problems := manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "redirect") {
		t.Fatalf("missing manifest problems = %v, want redirect", problems)
	}

	manifests["redirect"] = true
	manifests["not-a-plugin"] = true
	problems = manifestCoverageProblems(plugins, manifests)
	if len(problems) != 1 || !strings.Contains(problems[0], "not-a-plugin") {
		t.Fatalf("extra manifest problems = %v, want not-a-plugin", problems)
	}
}

func supportedPluginNames(data []byte) ([]string, error) {
	var plugins []string
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "|---") {
			continue
		}
		fields := strings.Split(strings.Trim(line, "|"), "|")
		if len(fields) < 6 || strings.TrimSpace(fields[3]) != "yes" ||
			!strings.HasPrefix(strings.TrimSpace(fields[5]), "Supported") {
			continue
		}
		match := documentedPluginName.FindStringSubmatch(fields[1])
		if len(match) != 2 {
			return nil, fmt.Errorf("supported plugin row has no backtick name: %s", line)
		}
		if seen[match[1]] {
			return nil, fmt.Errorf("supported plugin %q is duplicated", match[1])
		}
		seen[match[1]] = true
		plugins = append(plugins, match[1])
	}
	if len(plugins) == 0 {
		return nil, fmt.Errorf("no supported plugin rows found")
	}
	return plugins, nil
}

func manifestCoverageProblems(plugins []string, manifests map[string]bool) []string {
	selected := make(map[string]bool, len(plugins))
	var problems []string
	for _, pluginName := range plugins {
		selected[pluginName] = true
		if _, absent := upstreamSourceAbsences[pluginName]; absent {
			if manifests[pluginName] {
				problems = append(problems, fmt.Sprintf("source-absence plugin %s has a manifest", pluginName))
			}
			continue
		}
		if !manifests[pluginName] {
			problems = append(problems, fmt.Sprintf("supported plugin %s has no manifest", pluginName))
		}
	}
	for pluginName := range upstreamSourceAbsences {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("source-absence plugin %s is not selected", pluginName))
		}
	}
	for pluginName := range manifests {
		if !selected[pluginName] {
			problems = append(problems, fmt.Sprintf("manifest %s is not a supported plugin", pluginName))
		}
	}
	sort.Strings(problems)
	return problems
}
