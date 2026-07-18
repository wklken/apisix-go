package pluginintegration

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	Repository  string `yaml:"repository"`
	Commit      string `yaml:"commit"`
	File        string `yaml:"file"`
	Tests       int    `yaml:"tests"`
	TestNumbers []int  `yaml:"test_numbers,omitempty"`
}

type Case struct {
	Name          string          `yaml:"name"`
	Source        CaseSource      `yaml:"source"`
	Variants      []CaseVariant   `yaml:"variants,omitempty"`
	Environment   Environment     `yaml:"environment,omitempty"`
	Runtime       map[string]any  `yaml:"runtime,omitempty"`
	Config        map[string]any  `yaml:"config,omitempty"`
	Input         HTTPInput       `yaml:"input,omitempty"`
	Upstream      *UpstreamSpec   `yaml:"upstream,omitempty"`
	Output        HTTPOutput      `yaml:"output,omitempty"`
	Fixtures      []FixtureSpec   `yaml:"fixtures,omitempty"`
	Files         []ScenarioFile  `yaml:"files,omitempty"`
	Steps         []CaseStep      `yaml:"steps,omitempty"`
	TLS           *FrontendTLS    `yaml:"frontend_tls,omitempty"`
	AfterShutdown []FileAssertion `yaml:"after_shutdown,omitempty"`
}

type CaseVariant struct {
	Name          string          `yaml:"name"`
	Environment   Environment     `yaml:"environment,omitempty"`
	Runtime       map[string]any  `yaml:"runtime,omitempty"`
	Config        map[string]any  `yaml:"config,omitempty"`
	Input         HTTPInput       `yaml:"input,omitempty"`
	Upstream      *UpstreamSpec   `yaml:"upstream,omitempty"`
	Output        HTTPOutput      `yaml:"output,omitempty"`
	Fixtures      []FixtureSpec   `yaml:"fixtures,omitempty"`
	Files         []ScenarioFile  `yaml:"files,omitempty"`
	Steps         []CaseStep      `yaml:"steps,omitempty"`
	TLS           *FrontendTLS    `yaml:"frontend_tls,omitempty"`
	AfterShutdown []FileAssertion `yaml:"after_shutdown,omitempty"`
}

type FrontendTLS struct {
	SNI string `yaml:"sni"`
}

type Environment map[string]string

func (e *Environment) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return errors.New("environment must be a mapping")
	}

	environment := make(Environment, len(value.Content)/2)
	for i := 0; i < len(value.Content); i += 2 {
		name := value.Content[i]
		entry := value.Content[i+1]
		if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
			return errors.New("environment variable name must be a string")
		}
		if entry.Kind != yaml.ScalarNode || entry.Tag != "!!str" {
			return fmt.Errorf("environment variable %q value must be a string", name.Value)
		}
		environment[name.Value] = entry.Value
	}
	*e = environment
	return nil
}

type CaseStep struct {
	Name          string         `yaml:"name"`
	Repeat        int            `yaml:"repeat,omitempty"`
	Concurrency   int            `yaml:"concurrency,omitempty"`
	Config        map[string]any `yaml:"config,omitempty"`
	ConfigProbe   *ConfigProbe   `yaml:"config_probe,omitempty"`
	ConfigTimeout time.Duration  `yaml:"config_timeout,omitempty"`
	Input         HTTPInput      `yaml:"input"`
	Output        HTTPOutput     `yaml:"output"`
	Wait          time.Duration  `yaml:"wait,omitempty"`
}

type ConfigProbe struct {
	Input  HTTPInput  `yaml:"input"`
	Output HTTPOutput `yaml:"output"`
}

type ScenarioFile struct {
	Path string `yaml:"path"`
	Body string `yaml:"body"`
}

type FixtureSpec struct {
	Name           string                 `yaml:"name"`
	Kind           string                 `yaml:"kind"`
	ExpectRequests *int                   `yaml:"expect_requests,omitempty"`
	Expect         []HTTPAssertion        `yaml:"expect,omitempty"`
	Respond        []HTTPResponse         `yaml:"respond,omitempty"`
	NetworkExpect  []NetworkAssertion     `yaml:"network_expect,omitempty"`
	NetworkRespond []NetworkResponse      `yaml:"network_respond,omitempty"`
	Count          *FixtureCountAssertion `yaml:"count,omitempty"`
}

type FixtureCountAssertion struct {
	AtLeast int           `yaml:"at_least"`
	AtMost  int           `yaml:"at_most"`
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

type GRPCMessage struct {
	MessageBase64 string `yaml:"message_base64"`
	Status        string `yaml:"status,omitempty"`
}

type NetworkAssertion struct {
	Payload          *Matcher                    `yaml:"payload,omitempty"`
	PayloadBase64    *Matcher                    `yaml:"payload_base64,omitempty"`
	JSONFields       []NetworkJSONFieldAssertion `yaml:"json_fields,omitempty"`
	ForbiddenMatches []string                    `yaml:"forbidden_matches,omitempty"`
}

type NetworkJSONFieldAssertion struct {
	Path    string  `yaml:"path"`
	Value   Matcher `yaml:"value,omitempty"`
	RFC3339 bool    `yaml:"rfc3339,omitempty"`
}

type NetworkResponse struct {
	Payload       string        `yaml:"payload,omitempty"`
	PayloadBase64 string        `yaml:"payload_base64,omitempty"`
	Close         bool          `yaml:"close,omitempty"`
	Delay         time.Duration `yaml:"delay,omitempty"`
}

type FileAssertion struct {
	Path *Matcher `yaml:"path"`
	Body *Matcher `yaml:"body"`
}

type CaseSource struct {
	File  string `yaml:"file,omitempty"`
	Tests []int  `yaml:"tests"`
}

type HTTPInput struct {
	Method         string              `yaml:"method,omitempty"`
	Scheme         string              `yaml:"scheme,omitempty"`
	Version        string              `yaml:"version,omitempty"`
	Path           string              `yaml:"path"`
	Headers        map[string]string   `yaml:"headers,omitempty"`
	HeaderValues   map[string][]string `yaml:"header_values,omitempty"`
	Body           string              `yaml:"body,omitempty"`
	BodyBase64     string              `yaml:"body_base64,omitempty"`
	BodyRepeat     *RepeatedBody       `yaml:"body_repeat,omitempty"`
	HMAC           *HMACSignature      `yaml:"hmac,omitempty"`
	Chunked        bool                `yaml:"chunked,omitempty"`
	WithoutCookies bool                `yaml:"without_cookies,omitempty"`
	GRPC           *GRPCMessage        `yaml:"grpc,omitempty"`
}

type HMACSignature struct {
	KeyID     string   `yaml:"key_id"`
	Secret    string   `yaml:"secret"`
	Algorithm string   `yaml:"algorithm,omitempty"`
	Headers   []string `yaml:"headers"`
}

type RepeatedBody struct {
	Value  string `yaml:"value"`
	Count  int    `yaml:"count"`
	Suffix string `yaml:"suffix,omitempty"`
}

type UpstreamSpec struct {
	TLS     bool          `yaml:"tls,omitempty"`
	Expect  HTTPAssertion `yaml:"expect,omitempty"`
	Respond HTTPResponse  `yaml:"respond,omitempty"`
}

type HTTPAssertion struct {
	Method         string                   `yaml:"method,omitempty"`
	Protocol       string                   `yaml:"protocol,omitempty"`
	Path           *Matcher                 `yaml:"path,omitempty"`
	Host           *Matcher                 `yaml:"host,omitempty"`
	Headers        map[string]Matcher       `yaml:"headers,omitempty"`
	Body           *Matcher                 `yaml:"body,omitempty"`
	LokiPush       *LokiPushAssertion       `yaml:"loki_push,omitempty"`
	SkyWalkingLogs *SkyWalkingLogsAssertion `yaml:"skywalking_logs,omitempty"`
	GRPC           *GRPCMessage             `yaml:"grpc,omitempty"`
}

type LokiPushAssertion struct {
	Streams []LokiStreamAssertion `yaml:"streams"`
}

type LokiStreamAssertion struct {
	Stream map[string]string    `yaml:"stream"`
	Values []LokiValueAssertion `yaml:"values"`
}

type LokiValueAssertion struct {
	Entry  map[string]any `yaml:"entry"`
	Absent []string       `yaml:"absent,omitempty"`
}

type SkyWalkingLogsAssertion struct {
	Entries []SkyWalkingLogAssertion `yaml:"entries"`
}

type SkyWalkingLogAssertion struct {
	Service            Matcher                          `yaml:"service"`
	ServiceInstance    Matcher                          `yaml:"service_instance"`
	Endpoint           Matcher                          `yaml:"endpoint"`
	TraceContext       *SkyWalkingTraceContextAssertion `yaml:"trace_context,omitempty"`
	TraceContextAbsent bool                             `yaml:"trace_context_absent,omitempty"`
	Payload            map[string]Matcher               `yaml:"payload"`
	PayloadAbsent      []string                         `yaml:"payload_absent,omitempty"`
}

type SkyWalkingTraceContextAssertion struct {
	TraceID        string `yaml:"trace_id"`
	TraceSegmentID string `yaml:"trace_segment_id"`
	SpanID         int    `yaml:"span_id"`
}

type HTTPResponse struct {
	Status          int               `yaml:"status,omitempty"`
	Headers         map[string]string `yaml:"headers,omitempty"`
	Body            string            `yaml:"body,omitempty"`
	Chunks          []string          `yaml:"chunks,omitempty"`
	EchoRequestBody bool              `yaml:"echo_request_body,omitempty"`
	Delay           time.Duration     `yaml:"delay,omitempty"`
	GRPC            *GRPCMessage      `yaml:"grpc,omitempty"`
}

type HTTPOutput struct {
	Status                  int                      `yaml:"status"`
	Headers                 map[string]Matcher       `yaml:"headers,omitempty"`
	UniqueHeaders           []string                 `yaml:"unique_headers,omitempty"`
	MonotonicHeaders        []string                 `yaml:"monotonic_headers,omitempty"`
	DifferentHeaders        [][]string               `yaml:"different_headers,omitempty"`
	Body                    *Matcher                 `yaml:"body,omitempty"`
	GzipBody                *Matcher                 `yaml:"gzip_body,omitempty"`
	BrotliBody              *Matcher                 `yaml:"brotli_body,omitempty"`
	Logs                    *Matcher                 `yaml:"logs,omitempty"`
	SaveBodyLength          string                   `yaml:"save_body_length,omitempty"`
	BodyLengthLessThan      string                   `yaml:"body_length_less_than,omitempty"`
	BodyLengthLessThanValue *int                     `yaml:"body_length_less_than_value,omitempty"`
	ElapsedAtLeast          time.Duration            `yaml:"elapsed_at_least,omitempty"`
	ElapsedLessThan         time.Duration            `yaml:"elapsed_less_than,omitempty"`
	Captures                map[string]HeaderCapture `yaml:"captures,omitempty"`
	GRPC                    *GRPCMessage             `yaml:"grpc,omitempty"`
}

type HeaderCapture struct {
	Header  string `yaml:"header"`
	Matches string `yaml:"matches"`
}

type Matcher struct {
	Equals     *string  `yaml:"equals,omitempty"`
	JSONEquals *string  `yaml:"json_equals,omitempty"`
	Matches    *string  `yaml:"matches,omitempty"`
	NotMatches *string  `yaml:"not_matches,omitempty"`
	Absent     *bool    `yaml:"absent,omitempty"`
	Values     []string `yaml:"values,omitempty"`
}

type matcherScope string

const (
	matcherBody           matcherScope = "body"
	matcherHeader         matcherScope = "header"
	matcherPath           matcherScope = "path"
	matcherHost           matcherScope = "host"
	matcherLogs           matcherScope = "logs"
	matcherNetworkPayload matcherScope = "network payload"
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
		if len(source.TestNumbers) > 0 {
			if len(source.TestNumbers) != source.Tests {
				return fmt.Errorf("source %q test_numbers must contain %d entries", source.File, source.Tests)
			}
			seen := make(map[int]bool, len(source.TestNumbers))
			for _, number := range source.TestNumbers {
				if number <= 0 || seen[number] {
					return fmt.Errorf("source %q test_numbers must be unique and positive", source.File)
				}
				seen[number] = true
			}
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
			if !sourceHasTest(source, number) {
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
		numbers := source.TestNumbers
		if len(numbers) == 0 {
			numbers = make([]int, source.Tests)
			for i := range numbers {
				numbers[i] = i + 1
			}
		}
		for _, number := range numbers {
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

func sourceHasTest(source SourceSpec, number int) bool {
	if len(source.TestNumbers) == 0 {
		return number >= 1 && number <= source.Tests
	}
	return slices.Contains(source.TestNumbers, number)
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
	return len(c.Environment) > 0 || len(c.Runtime) > 0 || len(c.Config) > 0 || c.Input.Method != "" ||
		c.Input.Path != "" ||
		len(c.Input.Headers) > 0 ||
		len(c.Input.HeaderValues) > 0 ||
		c.Input.Body != "" ||
		c.Input.BodyBase64 != "" ||
		c.Input.BodyRepeat != nil ||
		c.Input.GRPC != nil ||
		c.Input.Chunked ||
		c.Upstream != nil ||
		c.Output.Status != 0 ||
		len(c.Output.Headers) > 0 ||
		c.Output.Body != nil ||
		c.Output.GRPC != nil ||
		c.Output.Logs != nil ||
		len(c.Fixtures) > 0 ||
		c.Output.GzipBody != nil ||
		c.Output.BrotliBody != nil ||
		c.Output.SaveBodyLength != "" ||
		c.Output.BodyLengthLessThan != "" ||
		c.Output.BodyLengthLessThanValue != nil ||
		c.Output.ElapsedAtLeast > 0 ||
		c.Output.ElapsedLessThan > 0 ||
		len(c.Output.UniqueHeaders) > 0 ||
		len(c.Output.MonotonicHeaders) > 0 ||
		len(c.Output.DifferentHeaders) > 0 ||
		len(c.Output.Captures) > 0 ||
		len(c.Files) > 0 ||
		len(c.Steps) > 0 ||
		c.TLS != nil ||
		len(c.AfterShutdown) > 0
}

func (v *CaseVariant) caseSpec() *Case {
	return &Case{
		Name:          v.Name,
		Environment:   v.Environment,
		Runtime:       v.Runtime,
		Config:        v.Config,
		Input:         v.Input,
		Upstream:      v.Upstream,
		Output:        v.Output,
		Fixtures:      v.Fixtures,
		Files:         v.Files,
		Steps:         v.Steps,
		TLS:           v.TLS,
		AfterShutdown: v.AfterShutdown,
	}
}

func (c *Case) validateScenario() error {
	if len(c.Config) == 0 {
		return errors.New("config is required")
	}
	if err := validateEnvironment(c.Environment); err != nil {
		return err
	}
	if err := validateScenarioFiles(c.Files); err != nil {
		return err
	}
	if c.TLS != nil && strings.TrimSpace(c.TLS.SNI) == "" {
		return errors.New("frontend TLS SNI is required")
	}
	if len(c.Steps) > 0 || len(c.Fixtures) > 0 {
		if c.Input.Method != "" || c.Input.Path != "" || len(c.Input.Headers) > 0 ||
			len(c.Input.HeaderValues) > 0 || c.Input.Body != "" || c.Input.BodyBase64 != "" ||
			c.Input.BodyRepeat != nil || c.Input.GRPC != nil ||
			c.Input.Chunked ||
			c.Upstream != nil || c.Output.Status != 0 || len(c.Output.Headers) > 0 || c.Output.Body != nil || c.Output.GRPC != nil ||
			c.Output.GzipBody != nil || c.Output.BrotliBody != nil || c.Output.Logs != nil || c.Output.SaveBodyLength != "" ||
			c.Output.BodyLengthLessThan != "" || c.Output.BodyLengthLessThanValue != nil || c.Output.ElapsedAtLeast > 0 || c.Output.ElapsedLessThan > 0 || len(c.Output.UniqueHeaders) > 0 ||
			len(c.Output.MonotonicHeaders) > 0 || len(c.Output.DifferentHeaders) > 0 ||
			len(c.Output.Captures) > 0 {
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
		if err := validateAfterShutdown(c.AfterShutdown); err != nil {
			return err
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
			if step.Concurrency < 0 || step.Concurrency > 64 {
				return fmt.Errorf("step %q concurrency must be between 0 and 64", step.Name)
			}
			if step.Concurrency > 0 && step.Repeat <= 0 {
				return fmt.Errorf("step %q concurrency requires a positive repeat", step.Name)
			}
			if step.Concurrency > 0 && len(step.Config) > 0 {
				return fmt.Errorf("step %q concurrency must not be combined with config update", step.Name)
			}
			if step.Concurrency > 0 && len(step.Output.Captures) > 0 {
				return fmt.Errorf("step %q concurrency must not be combined with output captures", step.Name)
			}
			if step.ConfigTimeout < 0 {
				return fmt.Errorf("step %q config_timeout must not be negative", step.Name)
			}
			if step.ConfigTimeout > 0 && len(step.Config) == 0 {
				return fmt.Errorf("step %q config_timeout requires config", step.Name)
			}
			if len(step.Config) > 0 && step.ConfigProbe == nil {
				return fmt.Errorf("step %q config_probe is required with config", step.Name)
			}
			if step.ConfigProbe != nil {
				if len(step.Config) == 0 {
					return fmt.Errorf("step %q config_probe requires config", step.Name)
				}
				if err := validateHTTPScenario(step.ConfigProbe.Input, step.ConfigProbe.Output); err != nil {
					return fmt.Errorf("step %q config_probe: %w", step.Name, err)
				}
				if err := validateConfigProbeInput(step.ConfigProbe.Input); err != nil {
					return fmt.Errorf("step %q config_probe: %w", step.Name, err)
				}
				if err := validateConfigProbeOutput(step.ConfigProbe.Output); err != nil {
					return fmt.Errorf("step %q config_probe: %w", step.Name, err)
				}
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

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnvironment(environment Environment) error {
	for name := range environment {
		if strings.ContainsRune(name, '\x00') || strings.Contains(name, "=") ||
			!environmentNamePattern.MatchString(name) {
			return fmt.Errorf("environment variable name %q must be a nonempty POSIX-style name", name)
		}
	}
	return nil
}

func validateConfigProbeInput(input HTTPInput) error {
	if input.Version != "" {
		return errors.New("input version is not supported; config probes use HTTP/1.1")
	}
	if input.Scheme == "https" {
		return errors.New("input scheme https is not supported; config probes use plain HTTP")
	}
	if input.WithoutCookies {
		return errors.New("input without_cookies is not supported; config probes never use the client cookie jar")
	}
	return nil
}

func validateConfigProbeOutput(output HTTPOutput) error {
	if output.Logs != nil || output.SaveBodyLength != "" || output.BodyLengthLessThan != "" ||
		output.BodyLengthLessThanValue != nil || output.ElapsedAtLeast > 0 || output.ElapsedLessThan > 0 ||
		len(output.UniqueHeaders) > 0 || len(output.MonotonicHeaders) > 0 ||
		len(output.DifferentHeaders) > 0 || len(output.Captures) > 0 {
		return errors.New("output supports only status, headers, body, gzip_body, and brotli_body")
	}
	return nil
}

func validateScenarioFiles(files []ScenarioFile) error {
	seen := make(map[string]bool, len(files))
	for i, file := range files {
		path := strings.TrimSpace(file.Path)
		if path == "" {
			return fmt.Errorf("file %d path is required", i+1)
		}
		clean := filepath.Clean(path)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("file %d path %q must stay within the scenario work directory", i+1, path)
		}
		if seen[clean] {
			return fmt.Errorf("file path %q is duplicated", path)
		}
		seen[clean] = true
	}
	return nil
}

func (c *Case) validateSingleScenario() error {
	logOnly := c.Input.Path == "" && c.Output.Status == 0 && c.Output.Logs != nil
	if logOnly {
		if err := c.Output.Logs.validate(matcherLogs); err != nil {
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
	if err := validateAfterShutdown(c.AfterShutdown); err != nil {
		return err
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
	if input.Version != "" && input.Version != "1.0" && input.Version != "1.1" && input.Version != "2" {
		return fmt.Errorf("input version %q is not supported", input.Version)
	}
	if input.BodyBase64 != "" {
		if input.Body != "" || input.BodyRepeat != nil || input.GRPC != nil {
			return errors.New("input body_base64, body, body_repeat, and grpc are mutually exclusive")
		}
		if _, err := base64.StdEncoding.DecodeString(input.BodyBase64); err != nil {
			return fmt.Errorf("input body_base64: %w", err)
		}
	}
	if input.BodyRepeat != nil {
		if input.Body != "" || input.BodyBase64 != "" {
			return errors.New("input body and body_repeat must not both be configured")
		}
		if input.BodyRepeat.Value == "" {
			return errors.New("input body_repeat value must not be empty")
		}
		if input.BodyRepeat.Count <= 0 {
			return errors.New("input body_repeat count must be positive")
		}
	}
	if input.GRPC != nil {
		if input.Body != "" || input.BodyBase64 != "" || input.BodyRepeat != nil {
			return errors.New("input grpc must not be combined with body or body_repeat")
		}
		if err := input.GRPC.validate(false); err != nil {
			return fmt.Errorf("input grpc: %w", err)
		}
		if input.Version != "2" {
			return errors.New("input grpc requires HTTP/2")
		}
		if input.Method != http.MethodPost {
			return errors.New("input grpc requires POST")
		}
	}
	if input.HMAC != nil {
		if strings.TrimSpace(input.HMAC.KeyID) == "" || input.HMAC.Secret == "" {
			return errors.New("input hmac key_id and secret are required")
		}
		if input.HMAC.Algorithm != "" && input.HMAC.Algorithm != "hmac-sha256" {
			return fmt.Errorf("input hmac algorithm %q is not supported", input.HMAC.Algorithm)
		}
		if len(input.HMAC.Headers) == 0 {
			return errors.New("input hmac headers are required")
		}
		for _, header := range input.HMAC.Headers {
			if strings.TrimSpace(header) == "" {
				return errors.New("input hmac headers must not contain blanks")
			}
		}
		for name := range input.Headers {
			if strings.EqualFold(name, "Authorization") {
				return errors.New("input hmac and Authorization header must not both be configured")
			}
		}
		for name := range input.HeaderValues {
			if strings.EqualFold(name, "Authorization") {
				return errors.New("input hmac and Authorization header must not both be configured")
			}
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
	for name, capture := range output.Captures {
		if strings.TrimSpace(name) == "" {
			return errors.New("response capture name must not be blank")
		}
		if strings.TrimSpace(capture.Header) == "" {
			return fmt.Errorf("response capture %q header must not be blank", name)
		}
		pattern, err := regexp.Compile(capture.Matches)
		if err != nil {
			return fmt.Errorf("response capture %q matches: %w", name, err)
		}
		if pattern.NumSubexp() != 1 {
			return fmt.Errorf("response capture %q matches must contain exactly one capture group", name)
		}
	}
	if output.Body != nil {
		if err := output.Body.validate(matcherBody); err != nil {
			return fmt.Errorf("output body: %w", err)
		}
	}
	if output.GRPC != nil {
		if output.Body != nil || output.GzipBody != nil || output.BrotliBody != nil {
			return errors.New("output grpc and body assertions are mutually exclusive")
		}
		if err := output.GRPC.validate(true); err != nil {
			return fmt.Errorf("output grpc: %w", err)
		}
	}
	if output.GzipBody != nil {
		if output.Body != nil || output.BrotliBody != nil {
			return errors.New("output body, gzip_body, and brotli_body are mutually exclusive")
		}
		if err := output.GzipBody.validate(matcherBody); err != nil {
			return fmt.Errorf("output gzip body: %w", err)
		}
	}
	if output.BrotliBody != nil {
		if output.Body != nil || output.GzipBody != nil {
			return errors.New("output body, gzip_body, and brotli_body are mutually exclusive")
		}
		if err := output.BrotliBody.validate(matcherBody); err != nil {
			return fmt.Errorf("output brotli body: %w", err)
		}
	}
	if output.SaveBodyLength != "" && strings.TrimSpace(output.SaveBodyLength) == "" {
		return errors.New("save_body_length must not be blank")
	}
	if output.BodyLengthLessThan != "" && strings.TrimSpace(output.BodyLengthLessThan) == "" {
		return errors.New("body_length_less_than must not be blank")
	}
	if output.BodyLengthLessThanValue != nil && *output.BodyLengthLessThanValue <= 0 {
		return errors.New("body_length_less_than_value must be positive")
	}
	if output.ElapsedAtLeast < 0 {
		return errors.New("elapsed_at_least must not be negative")
	}
	if output.ElapsedLessThan < 0 {
		return errors.New("elapsed_less_than must not be negative")
	}
	if output.ElapsedAtLeast > 0 && output.ElapsedLessThan > 0 && output.ElapsedAtLeast >= output.ElapsedLessThan {
		return errors.New("elapsed_at_least must be less than elapsed_less_than")
	}
	if output.Logs != nil {
		if err := output.Logs.validate(matcherLogs); err != nil {
			return fmt.Errorf("output logs: %w", err)
		}
	}
	return nil
}

func (f *FixtureSpec) validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return errors.New("name is required")
	}
	supportedKinds := map[string]bool{
		"http": true, "https": true, "h2c": true, "tcp": true, "tls-tcp": true, "udp": true, "grpc": true,
		"redis": true, "redis-cluster": true, "redis-sentinel": true,
		"kafka": true, "dubbo": true, "ldap": true,
	}
	if !supportedKinds[f.Kind] {
		return fmt.Errorf("kind %q is not supported", f.Kind)
	}
	if f.Kind == "http" || f.Kind == "https" || f.Kind == "h2c" {
		if len(f.NetworkExpect) > 0 || len(f.NetworkRespond) > 0 {
			return fmt.Errorf("%s fixture must use expect/respond", f.Kind)
		}
		if len(f.Respond) == 0 {
			return errors.New("at least one response is required")
		}
		if f.ExpectRequests != nil && *f.ExpectRequests != len(f.Expect) {
			return fmt.Errorf("expect_requests must equal the %d configured expectations", len(f.Expect))
		}
		if f.ExpectRequests != nil && f.Count != nil {
			return errors.New("expect_requests and count must not both be configured")
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
		if f.Count != nil {
			if len(f.Expect) != 1 {
				return errors.New("count assertion requires exactly one request expectation")
			}
			if err := f.Count.validate(); err != nil {
				return fmt.Errorf("count: %w", err)
			}
		}
		return nil
	}
	if f.ExpectRequests != nil {
		return fmt.Errorf("%s fixture must not configure expect_requests", f.Kind)
	}
	if f.Count != nil {
		return fmt.Errorf("%s fixture does not support count assertions", f.Kind)
	}
	if len(f.Expect) > 0 || len(f.Respond) > 0 {
		return fmt.Errorf("%s fixture must use network_expect/network_respond", f.Kind)
	}
	if len(f.NetworkExpect) == 0 {
		return errors.New("at least one network expectation is required")
	}
	if len(f.NetworkRespond) == 0 {
		return errors.New("at least one network response is required")
	}
	if len(f.NetworkRespond) != len(f.NetworkExpect) {
		return errors.New("network_expect and network_respond must contain the same number of entries")
	}
	if f.Kind == "udp" {
		for i, response := range f.NetworkRespond {
			if response.Close {
				return fmt.Errorf("network response %d: UDP fixture cannot close a datagram connection", i+1)
			}
		}
	}
	for i, assertion := range f.NetworkExpect {
		if err := assertion.validate(); err != nil {
			return fmt.Errorf("network expectation %d: %w", i+1, err)
		}
	}
	for i, response := range f.NetworkRespond {
		if err := response.validate(); err != nil {
			return fmt.Errorf("network response %d: %w", i+1, err)
		}
	}
	return nil
}

func (c FixtureCountAssertion) validate() error {
	if c.AtLeast < 0 || c.AtMost <= 0 || c.AtMost < c.AtLeast {
		return errors.New("at_least and at_most must define a non-negative bounded range")
	}
	if c.Timeout < 0 || c.Timeout > 5*time.Second {
		return errors.New("timeout must be between 0 and 5s")
	}
	return nil
}

func (g GRPCMessage) validate(withStatus bool) error {
	if g.MessageBase64 == "" {
		return errors.New("message_base64 is required")
	}
	if _, err := base64.StdEncoding.DecodeString(g.MessageBase64); err != nil {
		return fmt.Errorf("message_base64: %w", err)
	}
	if !withStatus && g.Status != "" {
		return errors.New("status is only valid for a response")
	}
	if withStatus && g.Status != "" {
		status, err := strconv.Atoi(g.Status)
		if err != nil || status < 0 || status > 16 {
			return errors.New("status must be a gRPC status code between 0 and 16")
		}
	}
	return nil
}

func (a NetworkAssertion) validate() error {
	configured := 0
	if a.Payload != nil {
		configured++
	}
	if a.PayloadBase64 != nil {
		configured++
	}
	if len(a.JSONFields) > 0 {
		configured++
	}
	if configured != 1 {
		return errors.New("exactly one of payload, payload_base64, or json_fields is required")
	}
	for i, pattern := range a.ForbiddenMatches {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("forbidden match %d must not be empty", i+1)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("forbidden match %d: %w", i+1, err)
		}
	}
	if len(a.JSONFields) > 0 {
		for i, field := range a.JSONFields {
			if _, err := parseJSONPointer(field.Path); err != nil {
				return fmt.Errorf("json field %d path: %w", i+1, err)
			}
			if field.RFC3339 {
				if field.Value.configured() {
					return fmt.Errorf("json field %d must configure exactly one of value or rfc3339", i+1)
				}
				continue
			}
			if err := field.Value.validate(matcherNetworkPayload); err != nil {
				return fmt.Errorf("json field %d value: %w", i+1, err)
			}
		}
		return nil
	}
	matcher := a.Payload
	kind := "payload"
	if matcher == nil {
		matcher = a.PayloadBase64
		kind = "payload_base64"
	}
	if err := matcher.validate(matcherNetworkPayload); err != nil {
		return fmt.Errorf("network %s: %w", kind, err)
	}
	return nil
}

func (m Matcher) configured() bool {
	return m.Equals != nil || m.JSONEquals != nil || m.Matches != nil ||
		m.NotMatches != nil || m.Absent != nil || m.Values != nil
}

func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errors.New("must be a JSON pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	for i, raw := range parts {
		var decoded strings.Builder
		for offset := 0; offset < len(raw); offset++ {
			if raw[offset] != '~' {
				decoded.WriteByte(raw[offset])
				continue
			}
			if offset+1 >= len(raw) {
				return nil, errors.New("invalid JSON pointer escape at end of token")
			}
			offset++
			switch raw[offset] {
			case '0':
				decoded.WriteByte('~')
			case '1':
				decoded.WriteByte('/')
			default:
				return nil, fmt.Errorf("invalid JSON pointer escape ~%c", raw[offset])
			}
		}
		parts[i] = decoded.String()
	}
	return parts, nil
}

func (r NetworkResponse) validate() error {
	if r.Payload != "" && r.PayloadBase64 != "" {
		return errors.New("payload and payload_base64 must not both be configured")
	}
	if r.Delay < 0 {
		return errors.New("delay must not be negative")
	}
	return nil
}

func validateAfterShutdown(assertions []FileAssertion) error {
	for i, assertion := range assertions {
		if assertion.Path == nil || assertion.Path.Equals == nil {
			return fmt.Errorf("after_shutdown assertion %d path must use equals", i+1)
		}
		if err := assertion.Path.validate(matcherPath); err != nil {
			return fmt.Errorf("after_shutdown assertion %d path: %w", i+1, err)
		}
		if !strings.HasPrefix(*assertion.Path.Equals, "{{WORK_DIR}}/") {
			return fmt.Errorf("after_shutdown assertion %d path must begin with {{WORK_DIR}}/", i+1)
		}
		if assertion.Body == nil {
			return fmt.Errorf("after_shutdown assertion %d body is required", i+1)
		}
		if err := assertion.Body.validate(matcherBody); err != nil {
			return fmt.Errorf("after_shutdown assertion %d body: %w", i+1, err)
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
	configuredBodies := 0
	if r.Body != "" || len(r.Chunks) > 0 {
		configuredBodies++
	}
	if r.EchoRequestBody {
		configuredBodies++
	}
	if r.GRPC != nil {
		configuredBodies++
		if err := r.GRPC.validate(true); err != nil {
			return fmt.Errorf("grpc: %w", err)
		}
	}
	if configuredBodies > 1 || (r.Body != "" && len(r.Chunks) > 0) {
		return errors.New("body, chunks, echo_request_body, and grpc are mutually exclusive")
	}
	if r.Delay < 0 {
		return errors.New("delay must not be negative")
	}
	if r.Delay > 5*time.Second {
		return errors.New("delay must not exceed 5s")
	}
	return nil
}

func (a HTTPAssertion) validate() error {
	if a.Protocol != "" && a.Protocol != "HTTP/1.0" && a.Protocol != "HTTP/1.1" && a.Protocol != "HTTP/2.0" {
		return fmt.Errorf("upstream request protocol %q is not supported", a.Protocol)
	}
	if a.Path != nil {
		if err := a.Path.validate(matcherPath); err != nil {
			return fmt.Errorf("upstream request path: %w", err)
		}
	}
	if a.Host != nil {
		if err := a.Host.validate(matcherHost); err != nil {
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
	bodyAssertions := 0
	if a.Body != nil {
		bodyAssertions++
	}
	if a.LokiPush != nil {
		bodyAssertions++
	}
	if a.SkyWalkingLogs != nil {
		bodyAssertions++
	}
	if a.GRPC != nil {
		bodyAssertions++
	}
	if bodyAssertions > 1 {
		return errors.New("upstream request body, loki_push, skywalking_logs, and grpc are mutually exclusive")
	}
	if a.LokiPush != nil {
		if err := a.LokiPush.validate(); err != nil {
			return fmt.Errorf("upstream Loki push: %w", err)
		}
	}
	if a.SkyWalkingLogs != nil {
		if err := a.SkyWalkingLogs.validate(); err != nil {
			return fmt.Errorf("upstream SkyWalking logs: %w", err)
		}
	}
	if a.GRPC != nil {
		if err := a.GRPC.validate(false); err != nil {
			return fmt.Errorf("upstream gRPC request: %w", err)
		}
	}
	return nil
}

func (a LokiPushAssertion) validate() error {
	if len(a.Streams) == 0 {
		return errors.New("at least one stream is required")
	}
	for streamIndex, stream := range a.Streams {
		if len(stream.Values) == 0 {
			return fmt.Errorf("stream %d must contain at least one value", streamIndex+1)
		}
		for valueIndex, value := range stream.Values {
			if len(value.Entry) == 0 {
				return fmt.Errorf("stream %d value %d entry is required", streamIndex+1, valueIndex+1)
			}
			for _, path := range value.Absent {
				if strings.TrimSpace(path) == "" {
					return fmt.Errorf("stream %d value %d absent path must not be blank", streamIndex+1, valueIndex+1)
				}
			}
		}
	}
	return nil
}

func (a SkyWalkingLogsAssertion) validate() error {
	if len(a.Entries) == 0 {
		return errors.New("entries must not be empty")
	}
	for i, entry := range a.Entries {
		for _, field := range []struct {
			name    string
			matcher Matcher
		}{
			{"service", entry.Service},
			{"service_instance", entry.ServiceInstance},
			{"endpoint", entry.Endpoint},
		} {
			if err := field.matcher.validate(matcherBody); err != nil {
				return fmt.Errorf("entry %d %s: %w", i+1, field.name, err)
			}
		}
		if entry.TraceContext != nil && entry.TraceContextAbsent {
			return fmt.Errorf("entry %d trace_context and trace_context_absent are mutually exclusive", i+1)
		}
		if len(entry.Payload) == 0 {
			return fmt.Errorf("entry %d payload must not be empty", i+1)
		}
		for path, matcher := range entry.Payload {
			if strings.TrimSpace(path) == "" {
				return fmt.Errorf("entry %d payload path must not be empty", i+1)
			}
			if err := matcher.validate(matcherBody); err != nil {
				return fmt.Errorf("entry %d payload %q: %w", i+1, path, err)
			}
		}
		for _, path := range entry.PayloadAbsent {
			if strings.TrimSpace(path) == "" {
				return fmt.Errorf("entry %d absent payload path must not be empty", i+1)
			}
		}
	}
	return nil
}

func (a LokiPushAssertion) match(body string) error {
	var payload struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	if len(payload.Streams) != len(a.Streams) {
		return fmt.Errorf("streams = %d, want exactly %d", len(payload.Streams), len(a.Streams))
	}
	for streamIndex, expectedStream := range a.Streams {
		actualStream := payload.Streams[streamIndex]
		if !stringMapEqual(actualStream.Stream, expectedStream.Stream) {
			return fmt.Errorf(
				"stream %d labels = %#v, want %#v",
				streamIndex+1,
				actualStream.Stream,
				expectedStream.Stream,
			)
		}
		if len(actualStream.Values) != len(expectedStream.Values) {
			return fmt.Errorf(
				"stream %d values = %d, want exactly %d",
				streamIndex+1,
				len(actualStream.Values),
				len(expectedStream.Values),
			)
		}
		for valueIndex, expectedValue := range expectedStream.Values {
			actualValue := actualStream.Values[valueIndex]
			if len(actualValue) != 2 {
				return fmt.Errorf(
					"stream %d value %d has %d fields, want 2",
					streamIndex+1,
					valueIndex+1,
					len(actualValue),
				)
			}
			timestamp, ok := new(big.Int).SetString(actualValue[0], 10)
			if !ok || timestamp.Sign() <= 0 {
				return fmt.Errorf(
					"stream %d value %d timestamp %q is not a positive decimal",
					streamIndex+1,
					valueIndex+1,
					actualValue[0],
				)
			}
			actualEntry, err := decodeSemanticJSON(actualValue[1])
			if err != nil {
				return fmt.Errorf("decode stream %d value %d entry: %w", streamIndex+1, valueIndex+1, err)
			}
			expectedJSON, err := json.Marshal(expectedValue.Entry)
			if err != nil {
				return fmt.Errorf("encode stream %d value %d expectation: %w", streamIndex+1, valueIndex+1, err)
			}
			expectedEntry, err := decodeSemanticJSON(string(expectedJSON))
			if err != nil {
				return fmt.Errorf("decode stream %d value %d expectation: %w", streamIndex+1, valueIndex+1, err)
			}
			contains, err := semanticJSONContains(actualEntry, expectedEntry)
			if err != nil {
				return fmt.Errorf("compare stream %d value %d entry: %w", streamIndex+1, valueIndex+1, err)
			}
			if !contains {
				return fmt.Errorf(
					"stream %d value %d entry %s does not contain %s",
					streamIndex+1,
					valueIndex+1,
					actualValue[1],
					expectedJSON,
				)
			}
			for _, path := range expectedValue.Absent {
				if semanticJSONPathPresent(actualEntry, path) {
					return fmt.Errorf(
						"stream %d value %d entry path %q is present, want absent",
						streamIndex+1,
						valueIndex+1,
						path,
					)
				}
			}
		}
	}
	return nil
}

func (a SkyWalkingLogsAssertion) match(body string) error {
	type traceContext struct {
		TraceID        string `json:"traceId"`
		TraceSegmentID string `json:"traceSegmentId"`
		SpanID         int    `json:"spanId"`
	}
	type envelope struct {
		TraceContext *traceContext `json:"traceContext"`
		Body         struct {
			JSON struct {
				JSON string `json:"json"`
			} `json:"json"`
		} `json:"body"`
		Service         string `json:"service"`
		ServiceInstance string `json:"serviceInstance"`
		Endpoint        string `json:"endpoint"`
	}

	var actual []envelope
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&actual); err != nil {
		return fmt.Errorf("decode envelope array: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode envelope array: trailing JSON value")
		}
		return fmt.Errorf("decode envelope array trailing data: %w", err)
	}
	if len(actual) != len(a.Entries) {
		return fmt.Errorf("got %d entries, want %d", len(actual), len(a.Entries))
	}
	for i, expected := range a.Entries {
		entry := actual[i]
		for name, valueMatcher := range map[string]struct {
			value   string
			matcher Matcher
		}{
			"service":         {entry.Service, expected.Service},
			"serviceInstance": {entry.ServiceInstance, expected.ServiceInstance},
			"endpoint":        {entry.Endpoint, expected.Endpoint},
		} {
			if err := valueMatcher.matcher.match(valueMatcher.value, true); err != nil {
				return fmt.Errorf("entry %d %s: %w", i+1, name, err)
			}
		}
		if expected.TraceContextAbsent && entry.TraceContext != nil {
			return fmt.Errorf("entry %d traceContext = %#v, want absent", i+1, entry.TraceContext)
		}
		if expected.TraceContext != nil {
			if entry.TraceContext == nil {
				return fmt.Errorf("entry %d traceContext is absent", i+1)
			}
			if got, want := *entry.TraceContext, *expected.TraceContext; got.TraceID != want.TraceID ||
				got.TraceSegmentID != want.TraceSegmentID || got.SpanID != want.SpanID {
				return fmt.Errorf("entry %d traceContext = %#v, want %#v", i+1, got, want)
			}
		}

		var payload map[string]any
		payloadDecoder := json.NewDecoder(strings.NewReader(entry.Body.JSON.JSON))
		payloadDecoder.UseNumber()
		if err := payloadDecoder.Decode(&payload); err != nil {
			return fmt.Errorf("entry %d decode body.json.json: %w", i+1, err)
		}
		if err := payloadDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return fmt.Errorf("entry %d decode body.json.json: trailing JSON value", i+1)
			}
			return fmt.Errorf("entry %d decode body.json.json trailing data: %w", i+1, err)
		}
		for path, matcher := range expected.Payload {
			value, present := nestedJSONValue(payload, path)
			if !present {
				return fmt.Errorf("entry %d payload %q is absent", i+1, path)
			}
			valueString, err := semanticValueString(value)
			if err != nil {
				return fmt.Errorf("entry %d payload %q: %w", i+1, path, err)
			}
			if err := matcher.match(valueString, true); err != nil {
				return fmt.Errorf("entry %d payload %q: %w", i+1, path, err)
			}
		}
		for _, path := range expected.PayloadAbsent {
			if value, present := nestedJSONValue(payload, path); present {
				return fmt.Errorf("entry %d payload %q = %#v, want absent", i+1, path, value)
			}
		}
	}
	return nil
}

func stringMapEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		rightValue, ok := right[key]
		if !ok || rightValue != value {
			return false
		}
	}
	return true
}

func semanticJSONContains(actual, expected any) (bool, error) {
	expectedObject, expectedIsObject := expected.(map[string]any)
	if !expectedIsObject {
		return semanticJSONEqual(actual, expected)
	}
	actualObject, actualIsObject := actual.(map[string]any)
	if !actualIsObject {
		return false, nil
	}
	for key, expectedValue := range expectedObject {
		actualValue, ok := actualObject[key]
		if !ok {
			return false, nil
		}
		contains, err := semanticJSONContains(actualValue, expectedValue)
		if err != nil || !contains {
			return contains, err
		}
	}
	return true, nil
}

func semanticJSONPathPresent(value any, path string) bool {
	current := value
	for segment := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[segment]
		if !ok {
			return false
		}
	}
	return true
}

func nestedJSONValue(value map[string]any, path string) (any, bool) {
	var current any = value
	for part := range strings.SplitSeq(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func semanticValueString(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (m Matcher) validate(scope matcherScope) error {
	operations := 0
	if m.Equals != nil {
		operations++
	}
	if m.JSONEquals != nil {
		operations++
		if scope != matcherBody {
			return fmt.Errorf("json_equals is only valid for bodies, not %s fields", scope)
		}
		if _, err := decodeSemanticJSON(*m.JSONEquals); err != nil {
			return fmt.Errorf("invalid json_equals: %w", err)
		}
	}
	if m.Matches != nil {
		operations++
		if _, err := regexp.Compile(*m.Matches); err != nil {
			return fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	if m.NotMatches != nil {
		operations++
		if _, err := regexp.Compile(*m.NotMatches); err != nil {
			return fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	if m.Absent != nil {
		operations++
		if scope != matcherHeader {
			return errors.New("absent is only valid for headers")
		}
		if !*m.Absent {
			return errors.New("absent must be true")
		}
	}
	if m.Values != nil {
		operations++
		if scope != matcherHeader {
			return errors.New("values is only valid for headers")
		}
		if len(m.Values) == 0 {
			return errors.New("values must not be empty")
		}
	}
	if operations != 1 {
		return errors.New(
			"matcher must configure exactly one of equals, json_equals, matches, not_matches, absent, or values",
		)
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
	case m.JSONEquals != nil:
		got, err := decodeSemanticJSON(value)
		if err != nil {
			return fmt.Errorf("decode actual JSON: %w", err)
		}
		want, err := decodeSemanticJSON(*m.JSONEquals)
		if err != nil {
			return fmt.Errorf("decode expected JSON: %w", err)
		}
		equal, err := semanticJSONEqual(got, want)
		if err != nil {
			return fmt.Errorf("compare semantic JSON: %w", err)
		}
		if !equal {
			return fmt.Errorf("got JSON %s, want %s", value, *m.JSONEquals)
		}
	case m.Matches != nil:
		if !regexp.MustCompile(*m.Matches).MatchString(value) {
			return fmt.Errorf("value %q does not match %q", value, *m.Matches)
		}
	case m.NotMatches != nil:
		if regexp.MustCompile(*m.NotMatches).MatchString(value) {
			return fmt.Errorf("value %q unexpectedly matches %q", value, *m.NotMatches)
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

func decodeSemanticJSON(value string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return decoded, nil
}

func semanticJSONEqual(left, right any) (bool, error) {
	switch leftValue := left.(type) {
	case nil:
		return right == nil, nil
	case bool:
		rightValue, ok := right.(bool)
		return ok && leftValue == rightValue, nil
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue, nil
	case json.Number:
		rightValue, ok := right.(json.Number)
		if !ok {
			return false, nil
		}
		leftNumber, ok := new(big.Rat).SetString(leftValue.String())
		if !ok {
			return false, fmt.Errorf("invalid JSON number %q", leftValue)
		}
		rightNumber, ok := new(big.Rat).SetString(rightValue.String())
		if !ok {
			return false, fmt.Errorf("invalid JSON number %q", rightValue)
		}
		return leftNumber.Cmp(rightNumber) == 0, nil
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false, nil
		}
		for i := range leftValue {
			equal, err := semanticJSONEqual(leftValue[i], rightValue[i])
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false, nil
		}
		for key, leftField := range leftValue {
			rightField, ok := rightValue[key]
			if !ok {
				return false, nil
			}
			equal, err := semanticJSONEqual(leftField, rightField)
			if err != nil || !equal {
				return equal, err
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported decoded JSON value %T", left)
	}
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
