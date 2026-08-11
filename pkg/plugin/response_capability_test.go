package plugin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type responseTestConfig struct {
	stage  string
	header bool
	body   bool
}

type countingResponseTestConfig struct {
	descriptor base.BindingPhaseDescriptor
	calls      atomic.Int32
	fail       atomic.Bool
}

func (c *countingResponseTestConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return base.BindingPhaseDescriptor{}, errors.New("descriptor called unexpectedly")
	}
	return c.descriptor, nil
}

func (c responseTestConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return base.BindingPhaseDescriptor{
		RequestStage: c.stage,
		Header:       c.header,
		BufferedBody: c.body,
	}, nil
}

type responseTestPlugin struct {
	base.BasePlugin
	config   any
	request  func(http.ResponseWriter, *http.Request) base.RequestPhaseResult
	header   func(*http.Request, *base.ResponseState) error
	body     func(*http.Request, *base.ResponseState) error
	store    func(*http.Request, base.ResponseState) error
	eligible func(apisixctx.ResponseSource) bool
}

func newResponseTestPlugin(name string, priority int, config any) *responseTestPlugin {
	plugin := &responseTestPlugin{config: config}
	plugin.Name = name
	plugin.SetPriority(priority)
	return plugin
}

func (p *responseTestPlugin) Init() error                            { return nil }
func (p *responseTestPlugin) PostInit() error                        { return nil }
func (p *responseTestPlugin) Config() any                            { return p.config }
func (p *responseTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *responseTestPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	if p.request == nil {
		return base.ContinueRequest(r)
	}
	return p.request(w, r)
}

func (p *responseTestPlugin) RunHeaderFilter(r *http.Request, state *base.ResponseState) error {
	if p.header == nil {
		return nil
	}
	return p.header(r, state)
}

func (p *responseTestPlugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if p.body == nil {
		return nil
	}
	return p.body(r, state)
}

func (p *responseTestPlugin) RunFinalResponseStore(r *http.Request, state base.ResponseState) error {
	if p.store == nil {
		return nil
	}
	return p.store(r, state)
}

func (p *responseTestPlugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	if p.eligible == nil {
		return source == apisixctx.ResponseSourceUpstream
	}
	return p.eligible(source)
}

func checkedResponseBinding(
	t *testing.T,
	factory string,
	plugin Plugin,
	scope Scope,
	id string,
) Binding {
	t.Helper()
	binding, err := BindPluginChecked(factory, plugin, scope, ResourceProvenance{Kind: ResourceRoute, ID: id})
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", factory, err)
	}
	return binding
}

func TestMaterializeResponseBindingsUsesExactManifestAndPartitionOrder(t *testing.T) {
	global := newResponseTestPlugin("global", 100, responseTestConfig{stage: "none", header: true})
	merged := newResponseTestPlugin("merged", 200, responseTestConfig{stage: "none", body: true})
	plan, err := MaterializeResponseBindings(EffectiveBindingSet{
		global: []Binding{checkedResponseBinding(t, "echo", global, ScopeGlobal, "global")},
		merged: []Binding{checkedResponseBinding(t, "echo", merged, ScopeRoute, "route")},
	})
	if err != nil {
		t.Fatalf("MaterializeResponseBindings() error = %v", err)
	}
	if len(plan) != 2 || plan[0].Plugin != global || plan[0].Phases != ResponsePhaseHeader ||
		plan[1].Plugin != merged || plan[1].Phases != ResponsePhaseBufferedBody {
		t.Fatalf("materialized plan = %#v", plan)
	}
}

func TestMaterializeResponseBindingsUsesPrivateFactoryIdentity(t *testing.T) {
	config := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{
		RequestStage: "none",
		Header:       true,
	}}
	plugin := newResponseTestPlugin("not-echo", 1, config)
	binding := checkedResponseBinding(t, "echo", plugin, ScopeRoute, "route")
	config.calls.Store(0)
	plan, err := MaterializeResponseBindings(EffectiveBindingSet{merged: []Binding{binding}})
	if err != nil {
		t.Fatalf("MaterializeResponseBindings() error = %v", err)
	}
	if len(plan) != 1 || plan[0].factoryKey != "echo" || plan[0].Plugin.GetName() != "not-echo" {
		t.Fatalf("plan = %#v", plan)
	}
	if calls := config.calls.Load(); calls != 1 {
		t.Fatalf("DescribeBindingPhases calls = %d, want 1 per materialization", calls)
	}
}

func TestPlan15ManifestAndRegistryHaveExactTenIdentities(t *testing.T) {
	want := []string{
		"api-breaker", "body-transformer", "echo", "error-page", "exit-transformer",
		"graphql-proxy-cache", "proxy-cache", "response-rewrite",
		"serverless-post-function", "serverless-pre-function",
	}
	got := make([]string, 0, len(responseFactoryRegistry))
	for identity := range responseFactoryRegistry {
		got = append(got, identity)
		if _, ok := requestStageRegistry[identity]; !ok {
			t.Fatalf("request-stage registry missing %q", identity)
		}
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response registry = %v, want %v", got, want)
	}

	manifest, err := os.ReadFile(filepath.Join(
		"..", "..", "docs", "superpowers", "plans", "2026-08-10-plugin-capability-manifest.md",
	))
	if err != nil {
		t.Fatalf("read capability manifest: %v", err)
	}
	section := string(manifest)
	start := strings.Index(section, "## Plan 15 bounded response identities")
	if start < 0 {
		t.Fatal("capability manifest missing Plan 15 section")
	}
	section = section[start:]
	if end := strings.Index(section[len("## Plan 15 bounded response identities"):], "\n## "); end >= 0 {
		section = section[:len("## Plan 15 bounded response identities")+end]
	}
	manifestIdentities := make([]string, 0, len(want))
	for line := range strings.SplitSeq(section, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 5 || strings.TrimSpace(fields[1]) == "Identity" ||
			strings.TrimSpace(fields[3]) == "Primary plan" {
			continue
		}
		identity := strings.TrimSpace(fields[1])
		if identity != "" && identity != "---" {
			manifestIdentities = append(manifestIdentities, identity)
		}
	}
	slices.Sort(manifestIdentities)
	if !reflect.DeepEqual(manifestIdentities, want) {
		t.Fatalf("manifest identities = %v, want %v", manifestIdentities, want)
	}
}

func TestMaterializeResponseBindingsRejectsUndeclaredCallback(t *testing.T) {
	plugin := newResponseTestPlugin("unknown", 1, nil)
	_, err := MaterializeResponseBindings(EffectiveBindingSet{merged: []Binding{{
		Plugin: plugin, Scope: ScopeRoute, Stage: RequestStageLegacy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r1"}, factoryName: "unknown",
	}}})
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("MaterializeResponseBindings() error = %v, want undeclared callback", err)
	}
}
