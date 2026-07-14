package pluginintegration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Manifest struct {
	Source SourceSpec `yaml:"source"`
	Cases  []Case     `yaml:"cases"`
}

type SourceSpec struct {
	Repository string `yaml:"repository"`
	Commit     string `yaml:"commit"`
	File       string `yaml:"file"`
	Tests      int    `yaml:"tests"`
}

type Case struct {
	Name     string         `yaml:"name"`
	Source   CaseSource     `yaml:"source"`
	Skip     string         `yaml:"skip,omitempty"`
	Runtime  map[string]any `yaml:"runtime,omitempty"`
	Config   map[string]any `yaml:"config,omitempty"`
	Input    HTTPInput      `yaml:"input,omitempty"`
	Upstream *UpstreamSpec  `yaml:"upstream,omitempty"`
	Output   HTTPOutput     `yaml:"output,omitempty"`
}

type CaseSource struct {
	Tests []int `yaml:"tests"`
}

type HTTPInput struct {
	Method  string            `yaml:"method,omitempty"`
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    string            `yaml:"body,omitempty"`
}

type UpstreamSpec struct {
	TLS     bool          `yaml:"tls,omitempty"`
	Expect  HTTPAssertion `yaml:"expect,omitempty"`
	Respond HTTPResponse  `yaml:"respond,omitempty"`
}

type HTTPAssertion struct {
	Method  string             `yaml:"method,omitempty"`
	Path    *Matcher           `yaml:"path,omitempty"`
	Host    *Matcher           `yaml:"host,omitempty"`
	Headers map[string]Matcher `yaml:"headers,omitempty"`
	Body    *Matcher           `yaml:"body,omitempty"`
}

type HTTPResponse struct {
	Status  int               `yaml:"status,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    string            `yaml:"body,omitempty"`
}

type HTTPOutput struct {
	Status  int                `yaml:"status"`
	Headers map[string]Matcher `yaml:"headers,omitempty"`
	Body    *Matcher           `yaml:"body,omitempty"`
	Logs    *Matcher           `yaml:"logs,omitempty"`
}

type Matcher struct {
	Equals  *string `yaml:"equals,omitempty"`
	Matches *string `yaml:"matches,omitempty"`
	Absent  *bool   `yaml:"absent,omitempty"`
}

type matcherKind int

const (
	matcherBody matcherKind = iota
	matcherHeader
)

func loadManifest(name string, data []byte) (*Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple YAML documents are not supported", name)
		}
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	if err := manifest.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", name, err)
	}
	return &manifest, nil
}

func (m *Manifest) validate() error {
	if strings.TrimSpace(m.Source.Repository) == "" {
		return errors.New("source repository is required")
	}
	if strings.TrimSpace(m.Source.Commit) == "" {
		return errors.New("source commit is required")
	}
	if strings.TrimSpace(m.Source.File) == "" {
		return errors.New("source file is required")
	}
	if m.Source.Tests <= 0 {
		return errors.New("source tests must be positive")
	}
	if len(m.Cases) == 0 {
		return errors.New("at least one case is required")
	}

	names := make(map[string]struct{}, len(m.Cases))
	mapped := make(map[int]string, m.Source.Tests)
	for i := range m.Cases {
		current := &m.Cases[i]
		name := strings.TrimSpace(current.Name)
		if name == "" {
			return fmt.Errorf("case %d name is required", i+1)
		}
		if _, ok := names[name]; ok {
			return fmt.Errorf("case name %q is duplicated", name)
		}
		names[name] = struct{}{}
		if len(current.Source.Tests) == 0 {
			return fmt.Errorf("case %q source tests are required", name)
		}
		for _, number := range current.Source.Tests {
			if number < 1 || number > m.Source.Tests {
				return fmt.Errorf("case %q source test %d is outside 1..%d", name, number, m.Source.Tests)
			}
			if previous, ok := mapped[number]; ok {
				return fmt.Errorf("source test %d is mapped more than once by %q and %q", number, previous, name)
			}
			mapped[number] = name
		}
		if err := current.validate(); err != nil {
			return fmt.Errorf("case %q: %w", name, err)
		}
	}
	for number := 1; number <= m.Source.Tests; number++ {
		if _, ok := mapped[number]; !ok {
			return fmt.Errorf("missing source test %d", number)
		}
	}
	return nil
}

func (c *Case) validate() error {
	if c.Skip != "" {
		if strings.TrimSpace(c.Skip) == "" {
			return errors.New("skip reason must not be blank")
		}
		return nil
	}
	if len(c.Config) == 0 {
		return errors.New("config is required")
	}
	logOnly := c.Input.Path == "" && c.Output.Status == 0 && c.Output.Logs != nil
	if !logOnly {
		if c.Input.Path == "" && c.Output.Status == 0 {
			return errors.New("HTTP output or log assertion is required")
		}
		if !strings.HasPrefix(c.Input.Path, "/") {
			return errors.New("input path must begin with /")
		}
		if c.Output.Status < 100 || c.Output.Status > 599 {
			return errors.New("output status must be between 100 and 599")
		}
	}
	for name, matcher := range c.Output.Headers {
		if err := matcher.validate(matcherHeader); err != nil {
			return fmt.Errorf("output header %q: %w", name, err)
		}
	}
	if c.Output.Body != nil {
		if err := c.Output.Body.validate(matcherBody); err != nil {
			return fmt.Errorf("output body: %w", err)
		}
	}
	if c.Output.Logs != nil {
		if err := c.Output.Logs.validate(matcherBody); err != nil {
			return fmt.Errorf("output logs: %w", err)
		}
	}
	if c.Upstream != nil {
		if err := c.Upstream.validate(); err != nil {
			return err
		}
	}
	data, err := yaml.Marshal(c.Config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if bytes.Contains(data, []byte("{{UPSTREAM_ADDR}}")) && c.Upstream == nil {
		return errors.New("{{UPSTREAM_ADDR}} requires an upstream fixture")
	}
	return nil
}

func (u *UpstreamSpec) validate() error {
	if u.Respond.Status != 0 && (u.Respond.Status < 100 || u.Respond.Status > 599) {
		return errors.New("upstream response status must be between 100 and 599")
	}
	if u.Expect.Path != nil {
		if err := u.Expect.Path.validate(matcherBody); err != nil {
			return fmt.Errorf("upstream request path: %w", err)
		}
	}
	if u.Expect.Host != nil {
		if err := u.Expect.Host.validate(matcherBody); err != nil {
			return fmt.Errorf("upstream request host: %w", err)
		}
	}
	for name, matcher := range u.Expect.Headers {
		if err := matcher.validate(matcherHeader); err != nil {
			return fmt.Errorf("upstream request header %q: %w", name, err)
		}
	}
	if u.Expect.Body != nil {
		if err := u.Expect.Body.validate(matcherBody); err != nil {
			return fmt.Errorf("upstream request body: %w", err)
		}
	}
	return nil
}

func (m Matcher) validate(kind matcherKind) error {
	operations := 0
	if m.Equals != nil {
		operations++
	}
	if m.Matches != nil {
		operations++
		if _, err := regexp.Compile(*m.Matches); err != nil {
			return fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	if m.Absent != nil {
		operations++
		if kind != matcherHeader {
			return errors.New("absent is only valid for headers")
		}
		if !*m.Absent {
			return errors.New("absent must be true")
		}
	}
	if operations != 1 {
		return errors.New("matcher must configure exactly one of equals, matches, or absent")
	}
	return nil
}

func (m Matcher) match(value string, present bool) error {
	switch {
	case m.Equals != nil:
		if value != *m.Equals {
			return fmt.Errorf("got %q, want %q", value, *m.Equals)
		}
	case m.Matches != nil:
		if !regexp.MustCompile(*m.Matches).MatchString(value) {
			return fmt.Errorf("value %q does not match %q", value, *m.Matches)
		}
	case m.Absent != nil:
		if present {
			return fmt.Errorf("value is present with %q, want absent", value)
		}
	default:
		return errors.New("matcher has no operation")
	}
	return nil
}

func mergeMap(dst, src map[string]any) {
	for key, value := range src {
		sourceMap, sourceIsMap := value.(map[string]any)
		destinationMap, destinationIsMap := dst[key].(map[string]any)
		if sourceIsMap && destinationIsMap {
			mergeMap(destinationMap, sourceMap)
			continue
		}
		dst[key] = value
	}
}
