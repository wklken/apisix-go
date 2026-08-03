package redirect

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

const redirectSource = "t/plugin/redirect.t"

type redirectManifestCase struct {
	Name   string `yaml:"name"`
	Serial bool   `yaml:"serial"`
	Source struct {
		File  string `yaml:"file"`
		Tests []int  `yaml:"tests"`
	} `yaml:"source"`
	Runtime map[string]any `yaml:"runtime"`
	Output  struct {
		Headers map[string]struct {
			Equals  *string `yaml:"equals"`
			Matches *string `yaml:"matches"`
		} `yaml:"headers"`
	} `yaml:"output"`
}

func loadRedirectManifest(t *testing.T) ([]redirectManifestCase, string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "t", "plugin", "redirect.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var manifest struct {
		Source struct {
			Commit string `yaml:"commit"`
			File   string `yaml:"file"`
			Tests  int    `yaml:"tests"`
		} `yaml:"source"`
		Cases []redirectManifestCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if manifest.Source.File != redirectSource {
		t.Fatalf("source file = %q, want %s", manifest.Source.File, redirectSource)
	}
	if manifest.Source.Commit != "c3d7d5ec69774121f53d2e20d29d09c816795dd7" || manifest.Source.Tests != 48 {
		t.Fatalf(
			"pinned source = (%q, %d), want APISIX c3d7d5ec with 48 tests",
			manifest.Source.Commit,
			manifest.Source.Tests,
		)
	}
	return manifest.Cases, path
}

func caseBySourceTests(cases []redirectManifestCase, numbers ...int) redirectManifestCase {
	for _, testCase := range cases {
		matches := len(testCase.Source.Tests) == len(numbers)
		for i, number := range numbers {
			if testCase.Source.Tests[i] != number {
				matches = false
			}
		}
		if matches {
			return testCase
		}
	}
	return redirectManifestCase{}
}

func sslListenPorts(runtime map[string]any) []int {
	apisix, _ := runtime["apisix"].(map[string]any)
	if apisix == nil {
		return nil
	}
	ssl, _ := apisix["ssl"].(map[string]any)
	if ssl == nil {
		return nil
	}
	enabled, _ := ssl["enable"].(bool)
	if !enabled {
		return nil
	}
	listen, _ := ssl["listen"].([]any)
	ports := make([]int, 0, len(listen))
	for _, raw := range listen {
		entry, _ := raw.(map[string]any)
		if port, ok := entry["port"].(int); ok {
			ports = append(ports, port)
		}
	}
	return ports
}

func hasRedirectHTTPSPortAttr(runtime map[string]any) bool {
	pluginAttr, _ := runtime["plugin_attr"].(map[string]any)
	if pluginAttr == nil {
		return false
	}
	redirect, _ := pluginAttr["redirect"].(map[string]any)
	if redirect == nil {
		return false
	}
	_, ok := redirect["https_port"]
	return ok
}

func TestManifestMapsSSLListenTestsToExactListenAndLocation(t *testing.T) {
	cases, path := loadRedirectManifest(t)

	type expectation struct {
		name       string
		numbers    []int
		ports      []int
		location   string
		locationRe string
	}
	expectations := []expectation{
		{name: "ssl-listen-port", numbers: []int{19}, ports: []int{9445}, location: "https://foo.com:9445/hello"},
		{
			name:     "single-ssl-listen-port",
			numbers:  []int{20},
			ports:    []int{9443},
			location: "https://foo.com:9443/hello",
		},
		{
			name:       "multiple-ssl-listen-ports",
			numbers:    []int{21},
			ports:      []int{6443, 7443, 8443, 9443},
			locationRe: "^https://foo.com:[6-9]443/hello$",
		},
	}

	for _, want := range expectations {
		testCase := caseBySourceTests(cases, want.numbers...)
		if testCase.Name == "" {
			t.Fatalf("%s: no case maps source tests %v", path, want.numbers)
		}
		if testCase.Name != want.name {
			t.Errorf("case for tests %v has name %q, want %q", want.numbers, testCase.Name, want.name)
		}
		if !testCase.Serial {
			t.Errorf("case %q must run serially to bind fixed SSL ports", testCase.Name)
		}
		if ports := sslListenPorts(testCase.Runtime); len(ports) != len(want.ports) {
			t.Errorf("case %q apisix.ssl.listen = %v, want %v", testCase.Name, ports, want.ports)
		} else {
			for i, port := range want.ports {
				if ports[i] != port {
					t.Errorf("case %q apisix.ssl.listen[%d] = %d, want %d", testCase.Name, i, ports[i], port)
				}
			}
		}
		if hasRedirectHTTPSPortAttr(testCase.Runtime) {
			t.Errorf("case %q must not bypass apisix.ssl.listen with plugin_attr.redirect.https_port", testCase.Name)
		}
		location := testCase.Output.Headers["Location"]
		if want.location != "" {
			if location.Equals == nil || *location.Equals != want.location {
				t.Errorf("case %q Location equals = %v, want %q", testCase.Name, location.Equals, want.location)
			}
		} else {
			if location.Matches == nil || *location.Matches != want.locationRe {
				t.Errorf("case %q Location matches = %v, want %q", testCase.Name, location.Matches, want.locationRe)
			}
		}
	}
}
