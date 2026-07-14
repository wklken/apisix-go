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
	Source  SourceSpec   `yaml:"source,omitempty"`
	Sources []SourceSpec `yaml:"sources,omitempty"`
	Cases   []Case       `yaml:"cases"`
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
	File  string `yaml:"file,omitempty"`
	Tests []int  `yaml:"tests"`
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
	sources := make([]SourceSpec, 0, len(m.Sources)+1)
	if m.Source.File != "" || m.Source.Repository != "" || m.Source.Commit != "" || m.Source.Tests != 0 {
		sources = append(sources, m.Source)
	}
	sources = append(sources, m.Sources...)
	if len(sources) == 0 {
		return errors.New("at least one source is required")
	}
	sourceByFile := make(map[string]SourceSpec, len(sources))
	for i, source := range sources {
		if strings.TrimSpace(source.Repository) == "" {
			return fmt.Errorf("source %d repository is required", i+1)
		}
		if strings.TrimSpace(source.Commit) == "" {
			return fmt.Errorf("source %d commit is required", i+1)
		}
		if strings.TrimSpace(source.File) == "" {
			return fmt.Errorf("source %d file is required", i+1)
		}
		if source.Tests <= 0 {
			return fmt.Errorf("source %q tests must be positive", source.File)
		}
		if _, exists := sourceByFile[source.File]; exists {
			return fmt.Errorf("source file %q is duplicated", source.File)
		}
		sourceByFile[source.File] = source
	}
	if len(m.Cases) == 0 {
		return errors.New("at least one case is required")
	}

	names := make(map[string]struct{}, len(m.Cases))
	mapped := make(map[string]map[int]string, len(sourceByFile))
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
		file := strings.TrimSpace(current.Source.File)
		if file == "" {
			if len(sourceByFile) != 1 {
				return fmt.Errorf("case %q source file is required when multiple sources are configured", name)
			}
			for sourceFile := range sourceByFile {
				file = sourceFile
			}
			current.Source.File = file
		}
		source, ok := sourceByFile[file]
		if !ok {
			return fmt.Errorf("case %q references unknown source file %q", name, file)
		}
		if mapped[file] == nil {
			mapped[file] = make(map[int]string, source.Tests)
		}
		for _, number := range current.Source.Tests {
			if number < 1 || number > source.Tests {
				return fmt.Errorf("case %q source test %d is outside 1..%d for %s", name, number, source.Tests, file)
			}
			if previous, ok := mapped[file][number]; ok {
				if len(sourceByFile) == 1 {
					return fmt.Errorf("source test %d is mapped more than once by %q and %q", number, previous, name)
				}
				return fmt.Errorf(
					"source test %d in %s is mapped more than once by %q and %q",
					number,
					file,
					previous,
					name,
				)
			}
			mapped[file][number] = name
		}
		if err := current.validate(); err != nil {
			return fmt.Errorf("case %q: %w", name, err)
		}
	}
	for file, source := range sourceByFile {
		for number := 1; number <= source.Tests; number++ {
			if _, ok := mapped[file][number]; !ok {
				return fmt.Errorf("missing source test %d in %s", number, file)
			}
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
