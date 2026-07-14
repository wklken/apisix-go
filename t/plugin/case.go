package pluginintegration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

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
	Variants []CaseVariant  `yaml:"variants,omitempty"`
	Runtime  map[string]any `yaml:"runtime,omitempty"`
	Config   map[string]any `yaml:"config,omitempty"`
	Input    HTTPInput      `yaml:"input,omitempty"`
	Upstream *UpstreamSpec  `yaml:"upstream,omitempty"`
	Output   HTTPOutput     `yaml:"output,omitempty"`
	Fixtures []FixtureSpec  `yaml:"fixtures,omitempty"`
	Steps    []CaseStep     `yaml:"steps,omitempty"`
	TLS      *FrontendTLS   `yaml:"frontend_tls,omitempty"`
}

type CaseVariant struct {
	Name     string         `yaml:"name"`
	Runtime  map[string]any `yaml:"runtime,omitempty"`
	Config   map[string]any `yaml:"config,omitempty"`
	Input    HTTPInput      `yaml:"input,omitempty"`
	Upstream *UpstreamSpec  `yaml:"upstream,omitempty"`
	Output   HTTPOutput     `yaml:"output,omitempty"`
	Fixtures []FixtureSpec  `yaml:"fixtures,omitempty"`
	Steps    []CaseStep     `yaml:"steps,omitempty"`
	TLS      *FrontendTLS   `yaml:"frontend_tls,omitempty"`
}

type FrontendTLS struct {
	SNI string `yaml:"sni"`
}

type CaseStep struct {
	Name   string        `yaml:"name"`
	Repeat int           `yaml:"repeat,omitempty"`
	Input  HTTPInput     `yaml:"input"`
	Output HTTPOutput    `yaml:"output"`
	Wait   time.Duration `yaml:"wait,omitempty"`
}

type FixtureSpec struct {
	Name    string          `yaml:"name"`
	Kind    string          `yaml:"kind"`
	Expect  []HTTPAssertion `yaml:"expect,omitempty"`
	Respond []HTTPResponse  `yaml:"respond"`
}

type CaseSource struct {
	File  string `yaml:"file,omitempty"`
	Tests []int  `yaml:"tests"`
}

type HTTPInput struct {
	Method       string              `yaml:"method,omitempty"`
	Scheme       string              `yaml:"scheme,omitempty"`
	Version      string              `yaml:"version,omitempty"`
	Path         string              `yaml:"path"`
	Headers      map[string]string   `yaml:"headers,omitempty"`
	HeaderValues map[string][]string `yaml:"header_values,omitempty"`
	Body         string              `yaml:"body,omitempty"`
	BodyRepeat   *RepeatedBody       `yaml:"body_repeat,omitempty"`
	Chunked      bool                `yaml:"chunked,omitempty"`
}

type RepeatedBody struct {
	Value string `yaml:"value"`
	Count int    `yaml:"count"`
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
	Chunks  []string          `yaml:"chunks,omitempty"`
}

type HTTPOutput struct {
	Status             int                `yaml:"status"`
	Headers            map[string]Matcher `yaml:"headers,omitempty"`
	UniqueHeaders      []string           `yaml:"unique_headers,omitempty"`
	MonotonicHeaders   []string           `yaml:"monotonic_headers,omitempty"`
	DifferentHeaders   [][]string         `yaml:"different_headers,omitempty"`
	Body               *Matcher           `yaml:"body,omitempty"`
	GzipBody           *Matcher           `yaml:"gzip_body,omitempty"`
	Logs               *Matcher           `yaml:"logs,omitempty"`
	SaveBodyLength     string             `yaml:"save_body_length,omitempty"`
	BodyLengthLessThan string             `yaml:"body_length_less_than,omitempty"`
}

type Matcher struct {
	Equals  *string  `yaml:"equals,omitempty"`
	Matches *string  `yaml:"matches,omitempty"`
	Absent  *bool    `yaml:"absent,omitempty"`
	Values  []string `yaml:"values,omitempty"`
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
	singularConfigured := m.Source.Repository != "" || m.Source.Commit != "" ||
		m.Source.File != "" || m.Source.Tests != 0
	if singularConfigured && len(m.Sources) > 0 {
		return errors.New("exactly one of source or sources is required")
	}

	multiSourceForm := len(m.Sources) > 0
	sources := m.Sources
	if singularConfigured {
		sources = []SourceSpec{m.Source}
	}
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
		if _, ok := sourceByFile[source.File]; ok {
			return fmt.Errorf("source file %q is duplicated", source.File)
		}
		sourceByFile[source.File] = source
	}
	if len(m.Cases) == 0 {
		return errors.New("at least one case is required")
	}

	names := make(map[string]struct{}, len(m.Cases))
	mapped := make(map[string]map[int]string, len(sources))
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
			if len(sources) != 1 {
				return fmt.Errorf("case %q source file is required when multiple sources are configured", name)
			}
			file = sources[0].File
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
				if !multiSourceForm {
					return fmt.Errorf("case %q source test %d is outside 1..%d", name, number, source.Tests)
				}
				return fmt.Errorf("case %q source test %d is outside 1..%d for %s", name, number, source.Tests, file)
			}
			if previous, ok := mapped[file][number]; ok {
				if !multiSourceForm {
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
	for _, source := range sources {
		for number := 1; number <= source.Tests; number++ {
			if _, ok := mapped[source.File][number]; !ok {
				if !multiSourceForm {
					return fmt.Errorf("missing source test %d", number)
				}
				return fmt.Errorf("missing source test %d in %s", number, source.File)
			}
		}
	}
	return nil
}

func (c *Case) validate() error {
	if len(c.Variants) > 0 {
		if c.hasScenario() {
			return errors.New("case with variants must not declare an inline scenario")
		}
		names := make(map[string]struct{}, len(c.Variants))
		for i := range c.Variants {
			variant := &c.Variants[i]
			name := strings.TrimSpace(variant.Name)
			if name == "" {
				return fmt.Errorf("variant %d name is required", i+1)
			}
			if _, ok := names[name]; ok {
				return fmt.Errorf("variant name %q is duplicated", name)
			}
			names[name] = struct{}{}
			if err := variant.caseSpec().validateScenario(); err != nil {
				return fmt.Errorf("variant %q: %w", name, err)
			}
		}
		return nil
	}
	return c.validateScenario()
}

func (c *Case) hasScenario() bool {
	return len(c.Runtime) > 0 || len(c.Config) > 0 || c.Input.Method != "" || c.Input.Path != "" ||
		len(c.Input.Headers) > 0 || len(c.Input.HeaderValues) > 0 || c.Input.Body != "" ||
		c.Input.BodyRepeat != nil || c.Input.Chunked ||
		c.Upstream != nil || c.Output.Status != 0 ||
		len(c.Output.Headers) > 0 || c.Output.Body != nil || c.Output.Logs != nil || len(c.Fixtures) > 0 ||
		c.Output.GzipBody != nil || c.Output.SaveBodyLength != "" || c.Output.BodyLengthLessThan != "" ||
		len(c.Output.UniqueHeaders) > 0 || len(c.Output.MonotonicHeaders) > 0 ||
		len(c.Output.DifferentHeaders) > 0 ||
		len(c.Steps) > 0 || c.TLS != nil
}

func (v *CaseVariant) caseSpec() *Case {
	return &Case{
		Name:     v.Name,
		Runtime:  v.Runtime,
		Config:   v.Config,
		Input:    v.Input,
		Upstream: v.Upstream,
		Output:   v.Output,
		Fixtures: v.Fixtures,
		Steps:    v.Steps,
		TLS:      v.TLS,
	}
}

func (c *Case) validateScenario() error {
	if len(c.Config) == 0 {
		return errors.New("config is required")
	}
	if c.TLS != nil && strings.TrimSpace(c.TLS.SNI) == "" {
		return errors.New("frontend TLS SNI is required")
	}
	if len(c.Steps) > 0 || len(c.Fixtures) > 0 {
		if c.Input.Method != "" || c.Input.Path != "" || len(c.Input.Headers) > 0 ||
			len(c.Input.HeaderValues) > 0 || c.Input.Body != "" || c.Input.BodyRepeat != nil ||
			c.Input.Chunked ||
			c.Upstream != nil || c.Output.Status != 0 || len(c.Output.Headers) > 0 || c.Output.Body != nil ||
			c.Output.GzipBody != nil || c.Output.Logs != nil || c.Output.SaveBodyLength != "" ||
			c.Output.BodyLengthLessThan != "" || len(c.Output.UniqueHeaders) > 0 ||
			len(c.Output.MonotonicHeaders) > 0 || len(c.Output.DifferentHeaders) > 0 {
			return errors.New("steps and fixtures must not be mixed with input, upstream, or output")
		}
		if len(c.Steps) == 0 {
			return errors.New("at least one step is required with named fixtures")
		}
		fixtureNames := make(map[string]bool, len(c.Fixtures))
		for i := range c.Fixtures {
			fixture := &c.Fixtures[i]
			if err := fixture.validate(); err != nil {
				return fmt.Errorf("fixture %d: %w", i+1, err)
			}
			if fixtureNames[fixture.Name] {
				return fmt.Errorf("fixture name %q is duplicated", fixture.Name)
			}
			fixtureNames[fixture.Name] = true
		}
		stepNames := make(map[string]bool, len(c.Steps))
		for i := range c.Steps {
			step := &c.Steps[i]
			if strings.TrimSpace(step.Name) == "" {
				return fmt.Errorf("step %d name is required", i+1)
			}
			if stepNames[step.Name] {
				return fmt.Errorf("step name %q is duplicated", step.Name)
			}
			stepNames[step.Name] = true
			if step.Wait < 0 {
				return fmt.Errorf("step %q wait must not be negative", step.Name)
			}
			if step.Repeat < 0 {
				return fmt.Errorf("step %q repeat must not be negative", step.Name)
			}
			if err := validateHTTPScenario(step.Input, step.Output); err != nil {
				return fmt.Errorf("step %q: %w", step.Name, err)
			}
			if step.Input.Scheme == "https" && c.TLS == nil {
				return fmt.Errorf("step %q: HTTPS input requires frontend TLS", step.Name)
			}
		}
		return nil
	}
	return c.validateSingleScenario()
}

func (c *Case) validateSingleScenario() error {
	logOnly := c.Input.Path == "" && c.Output.Status == 0 && c.Output.Logs != nil
	if logOnly {
		if err := c.Output.Logs.validate(matcherBody); err != nil {
			return fmt.Errorf("output logs: %w", err)
		}
	} else {
		if err := validateHTTPScenario(c.Input, c.Output); err != nil {
			return err
		}
		if c.Input.Scheme == "https" && c.TLS == nil {
			return errors.New("HTTPS input requires frontend TLS")
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

func validateHTTPScenario(input HTTPInput, output HTTPOutput) error {
	if input.Path == "" && output.Status == 0 {
		return errors.New("HTTP output or log assertion is required")
	}
	if !strings.HasPrefix(input.Path, "/") {
		return errors.New("input path must begin with /")
	}
	if input.Scheme != "" && input.Scheme != "http" && input.Scheme != "https" {
		return fmt.Errorf("input scheme %q is not supported", input.Scheme)
	}
	if input.Version != "" && input.Version != "1.0" && input.Version != "1.1" {
		return fmt.Errorf("input version %q is not supported", input.Version)
	}
	if input.BodyRepeat != nil {
		if input.Body != "" {
			return errors.New("input body and body_repeat must not both be configured")
		}
		if input.BodyRepeat.Value == "" {
			return errors.New("input body_repeat value must not be empty")
		}
		if input.BodyRepeat.Count <= 0 {
			return errors.New("input body_repeat count must be positive")
		}
	}
	if output.Status < 100 || output.Status > 599 {
		return errors.New("output status must be between 100 and 599")
	}
	for name, matcher := range output.Headers {
		if err := matcher.validate(matcherHeader); err != nil {
			return fmt.Errorf("output header %q: %w", name, err)
		}
	}
	for _, name := range append(slices.Clone(output.UniqueHeaders), output.MonotonicHeaders...) {
		if strings.TrimSpace(name) == "" {
			return errors.New("generated header assertion must not be blank")
		}
	}
	for _, pair := range output.DifferentHeaders {
		if len(pair) != 2 || strings.TrimSpace(pair[0]) == "" || strings.TrimSpace(pair[1]) == "" {
			return errors.New("different_headers entries must contain two non-blank header names")
		}
	}
	if output.Body != nil {
		if err := output.Body.validate(matcherBody); err != nil {
			return fmt.Errorf("output body: %w", err)
		}
	}
	if output.GzipBody != nil {
		if output.Body != nil {
			return errors.New("output body and gzip_body must not both be configured")
		}
		if err := output.GzipBody.validate(matcherBody); err != nil {
			return fmt.Errorf("output gzip body: %w", err)
		}
	}
	if output.SaveBodyLength != "" && strings.TrimSpace(output.SaveBodyLength) == "" {
		return errors.New("save_body_length must not be blank")
	}
	if output.BodyLengthLessThan != "" && strings.TrimSpace(output.BodyLengthLessThan) == "" {
		return errors.New("body_length_less_than must not be blank")
	}
	if output.Logs != nil {
		if err := output.Logs.validate(matcherBody); err != nil {
			return fmt.Errorf("output logs: %w", err)
		}
	}
	return nil
}

func (f *FixtureSpec) validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return errors.New("name is required")
	}
	if f.Kind != "http" && f.Kind != "https" {
		return fmt.Errorf("kind %q is not supported", f.Kind)
	}
	if len(f.Respond) == 0 {
		return errors.New("at least one response is required")
	}
	for i := range f.Respond {
		if err := f.Respond[i].validate(); err != nil {
			return fmt.Errorf("response %d: %w", i+1, err)
		}
	}
	for i := range f.Expect {
		if err := f.Expect[i].validate(); err != nil {
			return fmt.Errorf("expectation %d: %w", i+1, err)
		}
	}
	return nil
}

func (u *UpstreamSpec) validate() error {
	if err := u.Respond.validate(); err != nil {
		return fmt.Errorf("upstream response: %w", err)
	}
	return u.Expect.validate()
}

func (r HTTPResponse) validate() error {
	if r.Status != 0 && (r.Status < 100 || r.Status > 599) {
		return errors.New("status must be between 100 and 599")
	}
	if r.Body != "" && len(r.Chunks) > 0 {
		return errors.New("body and chunks must not both be configured")
	}
	return nil
}

func (a HTTPAssertion) validate() error {
	if a.Path != nil {
		if err := a.Path.validate(matcherBody); err != nil {
			return fmt.Errorf("upstream request path: %w", err)
		}
	}
	if a.Host != nil {
		if err := a.Host.validate(matcherBody); err != nil {
			return fmt.Errorf("upstream request host: %w", err)
		}
	}
	for name, matcher := range a.Headers {
		if err := matcher.validate(matcherHeader); err != nil {
			return fmt.Errorf("upstream request header %q: %w", name, err)
		}
	}
	if a.Body != nil {
		if err := a.Body.validate(matcherBody); err != nil {
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
	if m.Values != nil {
		operations++
		if kind != matcherHeader {
			return errors.New("values is only valid for headers")
		}
		if len(m.Values) == 0 {
			return errors.New("values must not be empty")
		}
	}
	if operations != 1 {
		return errors.New("matcher must configure exactly one of equals, matches, absent, or values")
	}
	return nil
}

func (m Matcher) matchHeader(value string, values []string) error {
	if m.Values == nil {
		return m.match(value, len(values) > 0)
	}
	if !slices.Equal(values, m.Values) {
		return fmt.Errorf("got values %q, want %q", values, m.Values)
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
