package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type recordingWorkflowStopper struct {
	name  string
	order *[]string
}

func (s recordingWorkflowStopper) Stop() {
	*s.order = append(*s.order, s.name)
}

type scriptedWorkflowPreparedChild struct {
	factory string
	child   any
	name    string
	order   *[]string
	entered chan struct{}
	onClose func()
	close   sync.Once
}

func (child *scriptedWorkflowPreparedChild) Factory() string { return child.factory }
func (child *scriptedWorkflowPreparedChild) Instance() any   { return child.child }
func (child *scriptedWorkflowPreparedChild) Close() {
	child.close.Do(func() {
		if child.onClose != nil {
			child.onClose()
		}
		if child.entered != nil {
			select {
			case <-child.entered:
			default:
				close(child.entered)
			}
		}
		*child.order = append(*child.order, child.name)
		if stopper, ok := child.child.(interface{ Stop() }); ok {
			stopper.Stop()
		}
	})
}

type scriptedWorkflowPreparer struct {
	t            *testing.T
	failAt       int
	scoped       bool
	selfStopName string
	failErr      error
	factory      string
	closeEntered chan struct{}
	onPrepare    func()
	afterPrepare func(int)
	onClose      func()
	calls        []base.CompositeChildSpec
	closeOrder   []string
}

type workflowConsumerLookup struct {
	groups map[string]resource.ConsumerGroup
	calls  int
}

func (*workflowConsumerLookup) ConsumerByPluginKey(string, string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (*workflowConsumerLookup) ConsumerByID(string) (resource.Consumer, bool) {
	return resource.Consumer{}, false
}

func (lookup *workflowConsumerLookup) ConsumerGroupByID(id string) (resource.ConsumerGroup, bool) {
	lookup.calls++
	group, ok := lookup.groups[id]
	return group, ok
}

func (preparer *scriptedWorkflowPreparer) Prepare(
	ctx context.Context,
	access base.ScopedSecretAccess,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	if preparer.onPrepare != nil {
		preparer.onPrepare()
	}
	preparer.calls = append(preparer.calls, spec)
	index := len(preparer.calls) - 1
	if index == preparer.failAt {
		preparer.closeOrder = append(preparer.closeOrder, preparer.selfStopName)
		if preparer.failErr != nil {
			return nil, preparer.failErr
		}
		return nil, errors.New("scripted child preparation failed")
	}
	child, err := newWorkflowChild(spec.Factory)
	if err != nil {
		preparer.t.Fatalf("child construction error: %v", err)
	}
	if err := child.Init(); err != nil {
		preparer.t.Fatalf("child Init() error = %v", err)
	}
	if err := util.Parse(spec.Config, child.Config()); err != nil {
		preparer.t.Fatalf("parse child config: %v", err)
	}
	if preparer.scoped {
		childAccess, err := access.Child(spec.Factory)
		if err != nil {
			return nil, err
		}
		if err := base.MaterializeScopedCompositeChildSecrets(ctx, childAccess, child); err != nil {
			return nil, err
		}
	}
	if err := child.PostInit(); err != nil {
		preparer.t.Fatalf("child PostInit() error = %v", err)
	}
	factory := spec.Factory
	if preparer.factory != "" {
		factory = preparer.factory
	}
	if preparer.afterPrepare != nil {
		preparer.afterPrepare(index)
	}
	return &scriptedWorkflowPreparedChild{
		factory: factory,
		child:   child,
		name:    fmt.Sprintf("child-%d", index),
		order:   &preparer.closeOrder,
		entered: preparer.closeEntered,
		onClose: preparer.onClose,
	}, nil
}

type blockingWorkflowContextChild struct {
	entered chan struct{}
	release chan struct{}
	onSet   func()
}

func (*blockingWorkflowContextChild) Init() error       { return nil }
func (*blockingWorkflowContextChild) PostInit() error   { return nil }
func (*blockingWorkflowContextChild) Config() any       { return &struct{}{} }
func (*blockingWorkflowContextChild) GetSchema() string { return `{}` }
func (child *blockingWorkflowContextChild) SetResourceContext(resource.Route, resource.Service) {
	close(child.entered)
	if child.onSet != nil {
		child.onSet()
	}
	<-child.release
}

func validScopedLimitCountConfig() map[string]any {
	return map[string]any{
		"count":       2,
		"time_window": 60,
		"key":         "remote_addr",
	}
}

type workflowScopedSecretBroker struct {
	values map[string]string
}

func (*workflowScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*workflowScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *workflowScopedSecretBroker) ResolveScoped(
	_ context.Context,
	_ secret.Scope,
	raw string,
) (string, error) {
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return "remote_addr", nil
}

func (*workflowScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func newWorkflowScopedCapability(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	valueMaps ...map[string]string,
) (secret.GenerationCapability, secret.Scope, func()) {
	t.Helper()
	values := map[string]string{}
	if len(valueMaps) > 0 {
		maps.Copy(values, valueMaps[0])
	}
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id":      resourceID,
		"plugins": map[string]any{"workflow": config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(
		revision, []generation.Resource{{Key: key, Value: document}}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "workflow-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := secret.NewScopedMaterializer(
		&workflowScopedSecretBroker{values: values}, catalog,
	).RegisterCandidate(context.Background(), ticket, publication)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		_ = registration.Close(context.Background())
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     "workflow",
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close workflow scoped attempt: %v", err)
		}
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, scoped: true}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(t, 1, "test-route", cfg)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestSetResourceContextForwardsRouteScopeToChildren(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{{Actions: []Action{{
			Name: "limit-req",
			Config: map[string]any{
				"rate":  1,
				"burst": 0,
				"key":   "remote_addr",
			},
		}}}},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatal(err)
	}
	p.SetResourceContext(resource.Route{ID: "route-1"}, resource.Service{})
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}

	child := p.children[actionPosition{rule: 0, action: 0}]
	if child == nil {
		t.Fatal("expected materialized limit-req child")
	}
	if dump := fmt.Sprintf("%#v", child); !strings.Contains(dump, `routeID:"route-1"`) {
		t.Fatalf("child = %s, want route-scoped limit key", dump)
	}
}

func TestParseOfficialReturnActionArray(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"case": []any{[]any{"uri", "==", "/anything/rejected"}},
				"actions": []any{
					[]any{"return", map[string]any{"code": 403}},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Rules) != 1 || len(cfg.Rules[0].Actions) != 1 {
		t.Fatalf("rules = %#v, want one rule with one action", cfg.Rules)
	}
	action := cfg.Rules[0].Actions[0]
	if action.Name != "return" {
		t.Fatalf("action name = %q, want return", action.Name)
	}
	if action.Return.Code != http.StatusForbidden {
		t.Fatalf("return code = %d, want 403", action.Return.Code)
	}
}

func TestWorkflowRejectsDisabledNestedPluginBeforeConstruction(t *testing.T) {
	for _, actionName := range []string{"limit-req", "limit-conn", "limit-count"} {
		t.Run(actionName, func(t *testing.T) {
			p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
				Name:   actionName,
				Config: map[string]any{"key": "$ENV://WORKFLOW_DISABLED_CHILD_SECRET"},
			}}}}}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			p.SetPluginEnabledChecker(func(name string) bool { return name != actionName })

			err := p.ValidatePreMaterialization()
			if err == nil || !strings.Contains(err.Error(), actionName) || !strings.Contains(err.Error(), "disabled") {
				t.Fatalf(
					"ValidatePreMaterialization() error = %v, want disabled action rejection before construction",
					err,
				)
			}
			if p.children != nil || p.childStoppers != nil {
				t.Fatalf("disabled action retained child state: children=%v stoppers=%v", p.children, p.childStoppers)
			}
			if got := p.config.Rules[0].Actions[0].Config["key"]; got != "$ENV://WORKFLOW_DISABLED_CHILD_SECRET" {
				t.Fatalf("disabled action key = %v, want untouched secret reference", got)
			}
		})
	}

	var enabled Config
	if err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{"actions": []any{
				[]any{
					"limit-req",
					map[string]any{
						"rate":          1,
						"burst":         0,
						"key":           "remote_addr",
						"rejected_code": http.StatusTooManyRequests,
						"nodelay":       true,
					},
				},
				[]any{
					"limit-conn",
					map[string]any{
						"conn":               1,
						"burst":              0,
						"default_conn_delay": 0.1,
						"key":                "remote_addr",
						"rejected_code":      http.StatusTooManyRequests,
					},
				},
				[]any{
					"limit-count",
					map[string]any{
						"count":         1,
						"time_window":   60,
						"key":           "remote_addr",
						"rejected_code": http.StatusTooManyRequests,
					},
				},
			}},
		},
	}, &enabled); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	withEnabled := &Plugin{config: enabled}
	if err := withEnabled.Init(); err != nil {
		t.Fatalf("enabled Init() error = %v", err)
	}
	withEnabled.SetPluginEnabledChecker(func(string) bool { return true })
	enabledPreparer := &scriptedWorkflowPreparer{t: t, failAt: -1, scoped: true}
	withEnabled.SetDependencies(base.Dependencies{CompositeChildren: enabledPreparer})
	enabledCapability, enabledScope, enabledCleanup := newWorkflowScopedCapability(
		t, 1, "enabled-actions", withEnabled.config,
	)
	t.Cleanup(enabledCleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), enabledScope, enabledCapability, withEnabled,
	); err != nil {
		t.Fatalf("enabled MaterializePluginSecrets() error = %v", err)
	}
	if err := withEnabled.PostInit(); err != nil {
		t.Fatalf("enabled PostInit() error = %v", err)
	}

	returns := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name:   "return",
		Return: ReturnAction{Code: http.StatusForbidden},
	}}}}}}
	if err := returns.Init(); err != nil {
		t.Fatalf("return Init() error = %v", err)
	}
	returns.SetPluginEnabledChecker(func(string) bool { return false })
	if err := base.MaterializePluginSecrets(returns); err != nil {
		t.Fatalf("return MaterializePluginSecrets() error = %v", err)
	}
	if err := returns.PostInit(); err != nil {
		t.Fatalf("return PostInit() error = %v, want return action independent of plugin membership", err)
	}
}

func TestValidatePreMaterializationRejectsEveryInvalidLimitActionSchema(t *testing.T) {
	for _, actionName := range []string{"limit-req", "limit-conn", "limit-count"} {
		t.Run(actionName, func(t *testing.T) {
			p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
				Name: actionName, Config: map[string]any{},
			}}}}}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}

			err := p.ValidatePreMaterialization()
			if err == nil || !strings.Contains(err.Error(), actionName) {
				t.Fatalf("ValidatePreMaterialization() error = %v, want %s schema rejection", err, actionName)
			}
		})
	}
}

func TestHandlerReturnsConfiguredStatusForMatchingCase(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"uri", "==", "/anything/rejected"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything/rejected", nil)
	rr := httptest.NewRecorder()
	nextCalled := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if nextCalled {
		t.Fatal("next handler was called, want workflow return to stop request")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rejected by workflow") {
		t.Fatalf("body = %q, want workflow rejection message", rr.Body.String())
	}
}

func TestHandlerFallsThroughWhenNoCaseMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=allowed", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Called", "yes")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if rr.Header().Get("X-Next-Called") != "yes" {
		t.Fatal("next handler was not called")
	}
}

func TestHandlerUsesFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
			{
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusTeapot}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=blocked", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want first matching rule status 403", rr.Code)
	}
}

func TestHandlerSupportsRestyExpressionOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{
					"AND",
					[]any{"method", "in", []any{"GET", "HEAD"}},
					[]any{"http_x_stage", "~*", "^prod$"},
					[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
				},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-Stage", "PrOd")
	req.RemoteAddr = "192.0.2.40:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestValidatePreMaterializationRejectsUnsupportedAction(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{Name: "unsupported-action"}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.ValidatePreMaterialization()
	if err == nil || !strings.Contains(err.Error(), "unsupported workflow action") {
		t.Fatalf("ValidatePreMaterialization() error = %v, want unsupported workflow action error", err)
	}
}

func TestPostInitFailureRollsBackChildrenInReverseOrder(t *testing.T) {
	var order []string
	p := &Plugin{
		config: Config{Rules: []Rule{{Actions: []Action{{Name: "unsupported-action"}}}}},
		childStoppers: []workflowChildStopper{
			recordingWorkflowStopper{name: "first", order: &order},
			recordingWorkflowStopper{name: "second", order: &order},
		},
	}

	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want later initialization failure")
	}
	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("rollback order = %q, want second,first", got)
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("rollback retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestMaterializeFailureRollsBackEarlierChildOwner(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{
			Name: "limit-count",
			Config: map[string]any{
				"count":       1,
				"time_window": 60,
				"key":         "remote_addr",
			},
		},
		{
			Name: "limit-req",
			Config: map[string]any{
				"rate":  1,
				"burst": 0,
				"key":   "$ENV://WORKFLOW_UNOWNED_KEY",
			},
		},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want second child materialization failure")
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("materialization rollback retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestStopRetiresChildrenInReverseOrderAndIsIdempotent(t *testing.T) {
	var order []string
	p := &Plugin{
		children: map[actionPosition]workflowChild{},
		childStoppers: []workflowChildStopper{
			recordingWorkflowStopper{name: "first", order: &order},
			recordingWorkflowStopper{name: "second", order: &order},
		},
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			p.Stop()
		})
	}
	wait.Wait()
	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("Stop order = %q, want second,first exactly once", got)
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("Stop retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestValidatePreMaterializationValidatesTerminalReturnCode(t *testing.T) {
	for _, test := range []struct {
		code    int
		wantErr bool
	}{
		{code: http.StatusContinue - 1, wantErr: true},
		{code: http.StatusContinue, wantErr: true},
		{code: http.StatusEarlyHints, wantErr: true},
		{code: 199, wantErr: true},
		{code: http.StatusOK},
		{code: 599},
		{code: 600, wantErr: true},
	} {
		t.Run(fmt.Sprintf("code-%d", test.code), func(t *testing.T) {
			p := &Plugin{config: Config{
				Rules: []Rule{
					{Actions: []Action{{Name: "return", Return: ReturnAction{Code: test.code}}}},
				},
			}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			err := p.ValidatePreMaterialization()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "return action code") {
					t.Fatalf("ValidatePreMaterialization() error = %v, want return action code error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidatePreMaterialization() error = %v, want nil", err)
			}
		})
	}
}

func TestValidatePreMaterializationRejectsInvalidLimitCountActionBeforeSecretResolution(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{
				Name: "limit-count",
				Config: map[string]any{
					"count": 2,
					"key":   "$ENV://WORKFLOW_INVALID_LIMIT_COUNT_KEY",
				},
			}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.ValidatePreMaterialization()
	if err == nil || !strings.Contains(err.Error(), "time_window") {
		t.Fatalf("ValidatePreMaterialization() error = %v, want missing time_window before secret resolution", err)
	}
	if p.children != nil || p.childStoppers != nil {
		t.Fatalf("invalid action retained child state: children=%v stoppers=%v", p.children, p.childStoppers)
	}
	action := p.config.Rules[0].Actions[0]
	if action.limitReq != nil || action.limitConn != nil || action.limitCount != nil {
		t.Fatalf("invalid action retained runtime child: %#v", action)
	}
}

func TestMaterializeScopedSecretsUsesStableSiblingPositions(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})

	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}
	if len(preparer.calls) != 2 {
		t.Fatalf("Prepare calls = %d, want 2", len(preparer.calls))
	}
	if got := preparer.calls[0].Position; got != "workflow/rule/0/action/0" {
		t.Fatalf("first child position = %q", got)
	}
	if got := preparer.calls[1].Position; got != "workflow/rule/0/action/1" {
		t.Fatalf("second child position = %q", got)
	}
	if preparer.calls[0].Factory != "limit-count" || preparer.calls[1].Factory != "limit-count" {
		t.Fatalf(
			"child factories = %q/%q, want exact limit-count",
			preparer.calls[0].Factory,
			preparer.calls[1].Factory,
		)
	}
	p.Stop()
}

func TestMaterializeScopedSecretsFailureStopsEarlierChildrenInReverseOnce(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: 2, selfStopName: "third-self"}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil {
		t.Fatal("MaterializeScopedSecrets() error = nil, want third child failure")
	}
	p.Stop()
	p.Stop()
	if got := strings.Join(preparer.closeOrder, ","); got != "third-self,child-1,child-0" {
		t.Fatalf("cleanup order = %q, want third-self,child-1,child-0 exactly once", got)
	}
	for _, action := range p.config.Rules[0].Actions {
		if action.limitReq != nil || action.limitConn != nil || action.limitCount != nil {
			t.Fatalf("failed scoped preparation published runtime child: %#v", action)
		}
	}
}

func TestMaterializeScopedSecretsRejectsForeignFactoryAndClosesItOnce(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, factory: "limit-req"}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil {
		t.Fatal("MaterializeScopedSecrets() error = nil, want foreign factory rejection")
	}
	p.Stop()
	if got := strings.Join(preparer.closeOrder, ","); got != "child-0" {
		t.Fatalf("foreign child cleanup = %q, want child-0 exactly once", got)
	}
}

func TestMaterializeScopedSecretsRejectsAllInvalidChildrenBeforePreparation(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{
			Name: "limit-count",
			Config: map[string]any{
				"count":       2,
				"time_window": 60,
				"key":         "$ENV://WORKFLOW_FIRST_CHILD_KEY",
			},
		},
		{
			Name: "limit-count",
			Config: map[string]any{
				"count": 2,
				"key":   "remote_addr",
			},
		},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil || !strings.Contains(err.Error(), "time_window") {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want later schema rejection", err)
	}
	if len(preparer.calls) != 0 {
		t.Fatalf("Prepare calls = %d, want zero before complete validation", len(preparer.calls))
	}
	if got := p.config.Rules[0].Actions[0].Config["key"]; got != "$ENV://WORKFLOW_FIRST_CHILD_KEY" {
		t.Fatalf("first child config = %v, want untouched reference after validation failure", got)
	}
}

func newScopedWorkflowWithPreparer(
	t *testing.T,
	preparer base.CompositeChildPreparer,
) *Plugin {
	t.Helper()
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if scripted, ok := preparer.(*scriptedWorkflowPreparer); ok {
		scripted.scoped = true
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(t, 1, "workflow-route", p.config)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}

func startBlockingWorkflowRequest(t *testing.T, p *Plugin) (chan struct{}, <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	handler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.77:1234"
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-entered
	return release, done
}

func requireWorkflowOperationCompletes(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("%s deadlocked during synchronous lifecycle reentry", operation)
	}
}

func TestScopedWorkflowHandlerLeaseDefersCloseWithoutBlockingStop(t *testing.T) {
	closeEntered := make(chan struct{})
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, closeEntered: closeEntered}
	p := newScopedWorkflowWithPreparer(t, preparer)
	release, requestDone := startBlockingWorkflowRequest(t, p)
	started := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		close(started)
		p.Stop()
		close(stopDone)
	}()
	<-started
	requireWorkflowOperationCompletes(t, stopDone, "Stop")
	select {
	case <-closeEntered:
		t.Fatal("Stop closed the workflow child while its handler was active")
	default:
	}
	close(release)
	<-requestDone
	<-closeEntered
}

func TestScopedWorkflowHandlerLeaseDefersOldCloseWithoutBlockingRematerialization(t *testing.T) {
	closeEntered := make(chan struct{})
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, closeEntered: closeEntered}
	p := newScopedWorkflowWithPreparer(t, preparer)
	nextCapability, nextScope, nextCleanup := newWorkflowScopedCapability(
		t, 2, "workflow-route-next", p.config,
	)
	t.Cleanup(nextCleanup)
	release, requestDone := startBlockingWorkflowRequest(t, p)
	started := make(chan struct{})
	materializeDone := make(chan struct{})
	var materializeErr error
	go func() {
		close(started)
		materializeErr = base.MaterializeScopedPluginSecrets(
			context.Background(), nextScope, nextCapability, p,
		)
		close(materializeDone)
	}()
	<-started
	requireWorkflowOperationCompletes(t, materializeDone, "rematerialization")
	select {
	case <-closeEntered:
		t.Fatal("rematerialization closed the workflow child while its handler was active")
	default:
	}
	close(release)
	<-requestDone
	if materializeErr != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", materializeErr)
	}
	<-closeEntered
	p.Stop()
	if got := strings.Join(preparer.closeOrder, ","); got != "child-0,child-1" {
		t.Fatalf("retirement order = %q, want old then current exactly once", got)
	}
}

func TestScopedWorkflowHandlerLeaseAllowsPostInitAndResourceContextReentry(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation func(*Plugin) error
	}{
		{
			name: "PostInit",
			operation: func(p *Plugin) error {
				err := p.PostInit()
				if errors.Is(err, errWorkflowLifecycleBusy) {
					return nil
				}
				return err
			},
		},
		{
			name: "SetResourceContext",
			operation: func(p *Plugin) error {
				p.SetResourceContext(resource.Route{ID: "route-updated"}, resource.Service{})
				return nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
			p := newScopedWorkflowWithPreparer(t, preparer)
			release, requestDone := startBlockingWorkflowRequest(t, p)
			started := make(chan struct{})
			operationDone := make(chan struct{})
			var operationErr error
			go func() {
				close(started)
				operationErr = test.operation(p)
				close(operationDone)
			}()
			<-started
			requireWorkflowOperationCompletes(t, operationDone, test.name)
			close(release)
			<-requestDone
			if operationErr != nil {
				t.Fatalf("%s error = %v", test.name, operationErr)
			}
			p.Stop()
		})
	}
}

func TestScopedWorkflowActiveHandlerDefersResourceContextToNextGeneration(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, scoped: true}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(t, 1, "active-handler", p.config)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	p.SetResourceContext(resource.Route{ID: "route-current"}, resource.Service{})
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	current := p.children[actionPosition{rule: 0, action: 0}]
	if dump := fmt.Sprintf("%#v", current); !strings.Contains(dump, `routeID:"route-current"`) {
		t.Fatalf("current child = %s, want initial route context", dump)
	}

	release, requestDone := startBlockingWorkflowRequest(t, p)
	p.SetResourceContext(resource.Route{ID: "route-next"}, resource.Service{})
	if dump := fmt.Sprintf("%#v", current); strings.Contains(dump, `routeID:"route-next"`) ||
		!strings.Contains(dump, `routeID:"route-current"`) {
		t.Fatalf("active child = %s, want unchanged current route", dump)
	}
	close(release)
	<-requestDone

	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	next := p.children[actionPosition{rule: 0, action: 0}]
	if next == current {
		t.Fatal("rematerialization retained the previous child")
	}
	if dump := fmt.Sprintf("%#v", next); !strings.Contains(dump, `routeID:"route-next"`) {
		t.Fatalf("next child = %s, want deferred route context", dump)
	}
	p.Stop()
}

func TestScopedWorkflowContextUpdatePreventsNewHandlerLease(t *testing.T) {
	child := &blockingWorkflowContextChild{entered: make(chan struct{}), release: make(chan struct{})}
	runtimeConfig := Config{Rules: []Rule{{Actions: []Action{{
		Name: "return", Return: ReturnAction{Code: http.StatusForbidden},
	}}}}}
	generation := &workflowGeneration{
		config: runtimeConfig,
		children: map[actionPosition]workflowChild{
			{rule: 0, action: 0}: child,
		},
	}
	p := &Plugin{config: runtimeConfig, current: generation, children: generation.children}

	contextDone := make(chan struct{})
	go func() {
		defer close(contextDone)
		p.SetResourceContext(resource.Route{ID: "route-exclusive"}, resource.Service{})
	}()
	requireWorkflowOperationCompletes(t, child.entered, "context update entry")

	recorder := httptest.NewRecorder()
	downstreamCalled := make(chan struct{}, 1)
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		close(handlerStarted)
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			downstreamCalled <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-handlerStarted
	select {
	case <-handlerDone:
		t.Fatal("handler completed before the context update finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(child.release)
	requireWorkflowOperationCompletes(t, contextDone, "context update release")
	requireWorkflowOperationCompletes(t, handlerDone, "handler retry after context update")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status after context update = %d, want workflow %d", recorder.Code, http.StatusForbidden)
	}
	select {
	case <-downstreamCalled:
		t.Fatal("handler bypassed workflow policy during the context update")
	default:
	}
	p.Stop()
}

func TestScopedWorkflowContextUpdateStopOverlapWakesHandlerAfterSetter(t *testing.T) {
	child := &blockingWorkflowContextChild{entered: make(chan struct{}), release: make(chan struct{})}
	closeEntered := make(chan struct{})
	var closeOrder []string
	owner := &scriptedWorkflowPreparedChild{
		factory: "test", child: child, name: "context-child", order: &closeOrder, entered: closeEntered,
	}
	runtimeConfig := Config{Rules: []Rule{{Actions: []Action{{
		Name: "return", Return: ReturnAction{Code: http.StatusForbidden},
	}}}}}
	generation := &workflowGeneration{
		config: runtimeConfig,
		children: map[actionPosition]workflowChild{
			{rule: 0, action: 0}: child,
		},
		owners: []base.PreparedCompositeChild{owner},
	}
	p := &Plugin{
		config: runtimeConfig, current: generation, children: generation.children, childOwners: generation.owners,
	}

	contextDone := make(chan struct{})
	go func() {
		defer close(contextDone)
		p.SetResourceContext(resource.Route{ID: "route-exclusive"}, resource.Service{})
	}()
	requireWorkflowOperationCompletes(t, child.entered, "context update entry")

	recorder := httptest.NewRecorder()
	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		close(handlerStarted)
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-handlerStarted
	select {
	case <-handlerDone:
		t.Fatal("handler completed before the context update finished")
	case <-time.After(50 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		p.Stop()
	}()
	requireWorkflowOperationCompletes(t, stopDone, "Stop during context update")
	select {
	case <-closeEntered:
		t.Fatal("Stop closed the child before the context-update lease was released")
	default:
	}
	select {
	case <-handlerDone:
		t.Fatal("Stop woke the handler before the context setter finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(child.release)
	requireWorkflowOperationCompletes(t, contextDone, "context update release")
	requireWorkflowOperationCompletes(t, closeEntered, "retired context child close")
	requireWorkflowOperationCompletes(t, handlerDone, "handler retry after retired context update")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status after Stop = %d, want downstream %d", recorder.Code, http.StatusNoContent)
	}
	if got := strings.Join(closeOrder, ","); got != "context-child" {
		t.Fatalf("close order = %q, want context-child exactly once", got)
	}
}

func TestScopedWorkflowContextSetterAllowsLifecycleReentry(t *testing.T) {
	child := &blockingWorkflowContextChild{entered: make(chan struct{}), release: make(chan struct{})}
	closeEntered := make(chan struct{})
	var closeOrder []string
	owner := &scriptedWorkflowPreparedChild{
		factory: "test", child: child, name: "context-child", order: &closeOrder, entered: closeEntered,
	}
	runtimeConfig := Config{Rules: []Rule{{Actions: []Action{{
		Name: "return", Return: ReturnAction{Code: http.StatusForbidden},
	}}}}}
	generation := &workflowGeneration{
		config: runtimeConfig,
		children: map[actionPosition]workflowChild{
			{rule: 0, action: 0}: child,
		},
		owners: []base.PreparedCompositeChild{owner},
	}
	p := &Plugin{
		config: runtimeConfig, current: generation, children: generation.children, childOwners: generation.owners,
	}
	reentered := make(chan struct{})
	child.onSet = func() {
		p.Stop()
		p.SetResourceContext(resource.Route{ID: "route-after-stop"}, resource.Service{})
		_ = p.PostInit()
		close(reentered)
	}

	contextDone := make(chan struct{})
	go func() {
		defer close(contextDone)
		p.SetResourceContext(resource.Route{ID: "route-exclusive"}, resource.Service{})
	}()
	requireWorkflowOperationCompletes(t, child.entered, "context setter entry")
	requireWorkflowOperationCompletes(t, reentered, "context setter lifecycle reentry")
	select {
	case <-closeEntered:
		t.Fatal("reentrant Stop closed the child before the context-update lease was released")
	default:
	}
	close(child.release)
	requireWorkflowOperationCompletes(t, contextDone, "context setter release")
	requireWorkflowOperationCompletes(t, closeEntered, "reentrant Stop child close")
	if got := strings.Join(closeOrder, ","); got != "context-child" {
		t.Fatalf("close order = %q, want context-child exactly once", got)
	}
}

func TestMaterializeScopedSecretsRejectsNilAndPreCanceledContextForReturnOnlyWorkflow(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() context.Context
		want    error
	}{
		{name: "nil", context: func() context.Context { return nil }, want: errWorkflowChildPreparation},
		{
			name: "pre-canceled",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
				Name: "return", Return: ReturnAction{Code: http.StatusNoContent},
			}}}}}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := p.MaterializeScopedSecrets(test.context(), base.ScopedSecretAccess{})
			if !errors.Is(err, test.want) {
				t.Fatalf("MaterializeScopedSecrets() error = %v, want %v", err, test.want)
			}
			if p.current != nil {
				t.Fatal("canceled return-only workflow published a generation")
			}
		})
	}
}

func TestMaterializeScopedSecretsCancellationBeforeCommitKeepsOldGeneration(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	old := p.current

	ctx, cancel := context.WithCancel(context.Background())
	preparer.afterPrepare = func(index int) {
		if index == 3 {
			cancel()
		}
	}
	err := p.MaterializeScopedSecrets(ctx, base.ScopedSecretAccess{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want context.Canceled", err)
	}
	if p.current != old {
		t.Fatal("commit cancellation replaced the last-good generation")
	}
	if got := strings.Join(preparer.closeOrder, ","); got != "child-3,child-2" {
		t.Fatalf("staged cleanup order = %q, want child-3,child-2", got)
	}

	preparer.afterPrepare = nil
	p.Stop()
	if got := strings.Join(preparer.closeOrder, ","); got != "child-3,child-2,child-1,child-0" {
		t.Fatalf("final cleanup order = %q, want staged then old generation exactly once", got)
	}
}

func TestMaterializeScopedSecretsCancellationWinsTokenInvalidation(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	old := p.current

	ctx, cancel := context.WithCancel(context.Background())
	var invalidatingToken uint64
	preparer.afterPrepare = func(index int) {
		if index == 1 {
			cancel()
			_, invalidatingToken = p.beginPreparation()
		}
	}
	err := p.MaterializeScopedSecrets(ctx, base.ScopedSecretAccess{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want canonical context.Canceled", err)
	}
	if p.current != old {
		t.Fatal("cancellation plus token invalidation replaced the last-good generation")
	}
	if got := strings.Join(preparer.closeOrder, ","); got != "child-1" {
		t.Fatalf("staged cleanup order = %q, want child-1 exactly once", got)
	}

	p.finishPreparation(invalidatingToken)
	preparer.afterPrepare = nil
	p.Stop()
	if got := strings.Join(preparer.closeOrder, ","); got != "child-1,child-0" {
		t.Fatalf("final cleanup order = %q, want staged then old generation exactly once", got)
	}
}

func TestScopedWorkflowPrepareAllowsSynchronousLifecycleReentry(t *testing.T) {
	for _, test := range []struct {
		name    string
		reenter func(*Plugin)
	}{
		{
			name: "SetResourceContext",
			reenter: func(p *Plugin) {
				p.SetResourceContext(resource.Route{ID: "route-from-prepare"}, resource.Service{})
			},
		},
		{
			name: "Stop",
			reenter: func(p *Plugin) {
				p.Stop()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
			p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
				Name: "limit-count", Config: validScopedLimitCountConfig(),
			}}}}}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
			preparer.onPrepare = func() { test.reenter(p) }

			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
			}()
			requireWorkflowOperationCompletes(t, done, "Prepare -> "+test.name)
			p.Stop()
		})
	}
}

func TestScopedWorkflowCloseAllowsSynchronousLifecycleReentry(t *testing.T) {
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	preparer.onClose = func() {
		p.SetResourceContext(resource.Route{ID: "route-from-close"}, resource.Service{})
		p.Stop()
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Stop()
	}()
	requireWorkflowOperationCompletes(t, done, "Close -> lifecycle")
	if got := strings.Join(preparer.closeOrder, ","); got != "child-0" {
		t.Fatalf("close order = %q, want child-0 exactly once", got)
	}
}

func TestScopedWorkflowHandlerAllowsSynchronousLifecycleReentryAndDefersReverseClose(t *testing.T) {
	closeEntered := make(chan struct{})
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, closeEntered: closeEntered, scoped: true}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
		{Name: "limit-count", Config: validScopedLimitCountConfig()},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(t, 1, "handler-reentry", p.config)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}

	nextReturned := make(chan struct{})
	handler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		p.Stop()
		p.SetResourceContext(resource.Route{ID: "route-from-handler"}, resource.Service{})
		_ = p.PostInit()
		select {
		case <-closeEntered:
			t.Error("handler reentry closed children before the active lease was released")
		default:
		}
		close(nextReturned)
	}))
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = "192.0.2.88:1234"
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	requireWorkflowOperationCompletes(t, nextReturned, "child/downstream lifecycle reentry")
	requireWorkflowOperationCompletes(t, done, "handler lease release")
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("retired children were not closed after the last active lease")
	}
	if got := strings.Join(preparer.closeOrder, ","); got != "child-1,child-0" {
		t.Fatalf("close order = %q, want child-1,child-0 exactly once", got)
	}
}

func TestMaterializeScopedSecretsRedactsInjectedPreparerErrors(t *testing.T) {
	poison := "vault/path/password/WORKFLOW_POISON"
	preparer := &scriptedWorkflowPreparer{
		t: t, failAt: 0, selfStopName: "failed-self", failErr: errors.New(poison),
	}
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-count", Config: validScopedLimitCountConfig(),
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil || err.Error() != "workflow child preparation failed" || strings.Contains(err.Error(), poison) {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want fixed redacted failure", err)
	}

	preparer.failErr = fmt.Errorf("%s: %w", poison, context.Canceled)
	preparer.failAt = len(preparer.calls)
	err = p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err != context.Canceled {
		t.Fatalf("MaterializeScopedSecrets() cancellation = %v, want canonical context.Canceled", err)
	}
}

func TestValidatePreMaterializationRejectsLimitCountGroup(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{
				Name: "limit-count",
				Config: map[string]any{
					"count":       2,
					"time_window": 60,
					"group":       "services_1",
				},
			}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.ValidatePreMaterialization()
	if err == nil || !strings.Contains(err.Error(), "group is not supported") {
		t.Fatalf("ValidatePreMaterialization() error = %v, want unsupported group validation error", err)
	}
}

func TestHandlerRunsLimitCountAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-count",
						map[string]any{
							"count":         1,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestMaterializeSecretsOwnsNestedLimitCountReferenceBeforePostInit(t *testing.T) {
	t.Setenv("WORKFLOW_LIMIT_COUNT_KEY", "remote_addr")
	var cfg Config
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"actions": []any{[]any{
				"limit-count",
				map[string]any{
					"count":         1,
					"time_window":   60,
					"key":           "$ENV://WORKFLOW_LIMIT_COUNT_KEY",
					"rejected_code": http.StatusTooManyRequests,
				},
			}},
		}},
	}, &cfg); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, scoped: true}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(
		t, 1, "nested-limit-count", p.config,
		map[string]string{"$ENV://WORKFLOW_LIMIT_COUNT_KEY": "remote_addr"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	key, _ := p.config.Rules[0].Actions[0].Config["key"].(string)
	wantKey := fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte("remote_addr")))
	if key != wantKey {
		t.Fatalf("nested limit-count key = %q, want content descriptor %q", key, wantKey)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", got, http.StatusNoContent)
	}
	if got := request(); got != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want nested materialized key quota rejection", got)
	}
	p.Stop()
	if p.config.Rules[0].Actions[0].limitCount != nil {
		t.Fatal("Stop() retained nested limit-count runtime child")
	}
}

func TestNestedLimitCountValidatesRedisClusterReferenceBeforeDescriptorRewrite(t *testing.T) {
	t.Setenv("WORKFLOW_REDIS_CLUSTER_NODE", "127.0.0.1:6379")
	var cfg Config
	if err := util.Parse(
		map[string]any{
			"rules": []any{map[string]any{
				"actions": []any{[]any{
					"limit-count",
					map[string]any{
						"count":               1,
						"time_window":         60,
						"key":                 "remote_addr",
						"policy":              "redis-cluster",
						"redis_cluster_nodes": []any{"$ENV://WORKFLOW_REDIS_CLUSTER_NODE"},
						"redis_cluster_name":  "workflow-cluster",
					},
				}},
			}},
		},
		&cfg,
	); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	preparer := &scriptedWorkflowPreparer{t: t, failAt: -1, scoped: true}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	capabilityValue, scope, cleanup := newWorkflowScopedCapability(
		t, 1, "nested-redis-cluster", p.config,
		map[string]string{"$ENV://WORKFLOW_REDIS_CLUSTER_NODE": "127.0.0.1:6379"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	nodes, ok := p.config.Rules[0].Actions[0].Config["redis_cluster_nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("redis_cluster_nodes = %#v, want one safe descriptor", nodes)
	}
	descriptor, _ := nodes[0].(string)
	wantDescriptor := fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte("127.0.0.1:6379")))
	if descriptor != wantDescriptor {
		t.Fatalf("redis cluster descriptor = %q, want content descriptor %q", descriptor, wantDescriptor)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want original valid node reference accepted", err)
	}
	p.Stop()
}

func TestConsumerWorkflowLimitCountOverridesRouteLimitCount(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-count",
						map[string]any{
							"count":         5,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	nextCalled := false
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		if !apisixctx.ConsumerPluginOverrides(r, "limit-count") {
			t.Fatal("consumer workflow action did not override route limit-count")
		}
		if !apisixctx.ConsumerPluginOverrides(r, "consumer-restriction") {
			t.Fatal("consumer workflow action discarded another consumer plugin override")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.20:1234"
	req = apisixctx.WithApisixVars(req, nil)
	apisixctx.AttachConsumer(req, resource.Consumer{
		Username: "jack",
		Plugins: map[string]resource.PluginConfig{
			"consumer-restriction": map[string]any{},
			"workflow":             map[string]any{},
		},
	})
	req = apisixctx.WithConsumerPluginOverrides(req, map[string]struct{}{
		"consumer-restriction": {},
		"workflow":             {},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if !nextCalled {
		t.Fatal("next handler was not called")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestScopedWorkflowConsumerGroupLookupNeverFallsBackToStore(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := store.Open(
		filepath.Join(t.TempDir(), "workflow-consumer.db"),
		make(chan *store.Event),
		data_encryption.NewService(false, nil, catalog),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := storage.Close(); closeErr != nil {
			t.Fatalf("close store: %v", closeErr)
		}
	})
	if err := store.WriteBucketValueForTest(
		storage,
		"consumer_groups",
		"group-stale",
		[]byte(`{"id":"group-stale","plugins":{"stale-store-plugin":{}}}`),
	); err != nil {
		t.Fatal(err)
	}
	previous := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previous) })

	lookup := &workflowConsumerLookup{groups: map[string]resource.ConsumerGroup{}}
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = apisixctx.WithApisixVars(req, nil)
	apisixctx.AttachConsumer(req, resource.Consumer{
		Username: "jack",
		GroupID:  "group-stale",
		Plugins: map[string]resource.PluginConfig{
			"consumer-plugin": map[string]any{},
		},
	})
	req = apisixctx.WithConsumerPluginOverrides(req, map[string]struct{}{"workflow": {}})

	got := p.withConsumerActionOverride(req, "limit-count")
	if lookup.calls != 1 {
		t.Fatalf("immutable group lookup calls = %d, want 1", lookup.calls)
	}
	if apisixctx.ConsumerPluginOverrides(got, "stale-store-plugin") {
		t.Fatal("immutable lookup miss fell back to stale process-global Store")
	}
	if !apisixctx.ConsumerPluginOverrides(got, "consumer-plugin") ||
		!apisixctx.ConsumerPluginOverrides(got, "limit-count") {
		t.Fatal("consumer/action override union was not preserved")
	}
}

func TestHandlerRunsLimitReqAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-req",
						map[string]any{
							"rate":          1,
							"burst":         0,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
							"nodelay":       true,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.10:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.10:5678"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerRunsLimitConnAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-conn",
						map[string]any{
							"conn":               1,
							"burst":              0,
							"default_conn_delay": 0.1,
							"key":                "remote_addr",
							"rejected_code":      http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(firstRecorder, first)
	}()
	<-started

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:5678"
	secondRecorder := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		handler.ServeHTTP(secondRecorder, second)
	}()
	select {
	case <-secondDone:
	case <-time.After(200 * time.Millisecond):
		close(block)
		<-firstDone
		<-secondDone
		t.Fatal(
			"second request reached downstream, want workflow limit-conn action to reject while first request is active",
		)
	}
	if secondRecorder.Code != http.StatusTooManyRequests {
		close(block)
		<-firstDone
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}

	close(block)
	<-firstDone
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
}

func TestUnownedSecretReferenceRejectsNestedWorkflowPlugin(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-req",
		Config: map[string]any{
			"rate":  1,
			"burst": 0,
			"key":   "$ENV://WORKFLOW_KEY",
		},
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := base.MaterializePluginSecrets(p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("MaterializePluginSecrets() error = %v, want redacted nested secret rejection", err)
	}
}
