package multi_auth

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type scriptedChildPreparer struct {
	mu       sync.Mutex
	calls    []base.CompositeChildSpec
	children map[string]authPlugin
	failAt   int
	closed   *[]string
	prepare  func(base.CompositeChildSpec)
	close    func()
}

func (p *scriptedChildPreparer) replaceChildren(
	children map[string]authPlugin,
	closed *[]string,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.children = children
	p.closed = closed
}

func (p *scriptedChildPreparer) Prepare(
	_ context.Context,
	_ base.ScopedSecretAccess,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	p.mu.Lock()
	p.calls = append(p.calls, spec)
	callCount := len(p.calls)
	instance := p.children[spec.Position]
	closed := p.closed
	prepare := p.prepare
	closeHook := p.close
	failAt := p.failAt
	p.mu.Unlock()
	if prepare != nil {
		prepare(spec)
	}
	child := &scriptedPreparedChild{
		factory:   spec.Factory,
		instance:  instance,
		position:  spec.Position,
		closed:    closed,
		closeHook: closeHook,
	}
	if callCount == failAt {
		return child, errors.New("poison raw credential")
	}
	return child, nil
}

type scriptedPreparedChild struct {
	factory   string
	instance  any
	position  string
	closed    *[]string
	closeHook func()
	once      sync.Once
}

func (c *scriptedPreparedChild) Factory() string { return c.factory }
func (c *scriptedPreparedChild) Instance() any   { return c.instance }
func (c *scriptedPreparedChild) Close() {
	c.once.Do(func() {
		if c.closeHook != nil {
			c.closeHook()
		}
		if c.closed != nil {
			*c.closed = append(*c.closed, c.position)
		}
	})
}

type scriptedAuthPlugin struct {
	config map[string]any
}

func (*scriptedAuthPlugin) Init() error                            { return nil }
func (*scriptedAuthPlugin) PostInit() error                        { return nil }
func (p *scriptedAuthPlugin) Config() any                          { return &p.config }
func (*scriptedAuthPlugin) GetSchema() string                      { return "" }
func (*scriptedAuthPlugin) Handler(next http.Handler) http.Handler { return next }

type blockingAuthPlugin struct {
	config  map[string]any
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	run     func()
}

func (*blockingAuthPlugin) Init() error       { return nil }
func (*blockingAuthPlugin) PostInit() error   { return nil }
func (p *blockingAuthPlugin) Config() any     { return &p.config }
func (*blockingAuthPlugin) GetSchema() string { return "" }
func (*blockingAuthPlugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *blockingAuthPlugin) RunRequestPhase(
	_ http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	p.once.Do(func() { close(p.entered) })
	if p.run != nil {
		p.run()
	}
	<-p.release
	return base.StopRequest(r)
}

type reentrantHandlerAuthPlugin struct {
	config map[string]any
	run    func()
}

type callbackConfigAuthPlugin struct {
	config   map[string]any
	onConfig func()
	once     sync.Once
}

func (*callbackConfigAuthPlugin) Init() error     { return nil }
func (*callbackConfigAuthPlugin) PostInit() error { return nil }
func (p *callbackConfigAuthPlugin) Config() any {
	p.once.Do(p.onConfig)
	return &p.config
}
func (*callbackConfigAuthPlugin) GetSchema() string                      { return "" }
func (*callbackConfigAuthPlugin) Handler(next http.Handler) http.Handler { return next }

func (*reentrantHandlerAuthPlugin) Init() error       { return nil }
func (*reentrantHandlerAuthPlugin) PostInit() error   { return nil }
func (p *reentrantHandlerAuthPlugin) Config() any     { return &p.config }
func (*reentrantHandlerAuthPlugin) GetSchema() string { return "" }
func (p *reentrantHandlerAuthPlugin) Handler(http.Handler) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { p.run() })
}

func TestMaterializeScopedSecretsValidatesAllRawChildrenBeforeSecretAccess(t *testing.T) {
	preparer := &scriptedChildPreparer{}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {"header": 42}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil {
		t.Fatal("MaterializeScopedSecrets() error = nil, want invalid child schema")
	}
	if len(preparer.calls) != 0 {
		t.Fatalf("Prepare() calls = %d, want zero before all raw validation succeeds", len(preparer.calls))
	}
	if len(p.auths) != 0 {
		t.Fatalf("published auths = %d, want zero", len(p.auths))
	}
}

func TestMaterializeScopedSecretsUsesDeterministicPositions(t *testing.T) {
	closed := []string{}
	preparer := &scriptedChildPreparer{closed: &closed, children: map[string]authPlugin{}}
	wantPositions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/0/factory/key-auth",
		"multi-auth/entry/1/factory/jwt-auth",
	}
	for _, position := range wantPositions {
		preparer.children[position] = &scriptedAuthPlugin{config: map[string]any{"prepared": position}}
	}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"key-auth": {}, "basic-auth": {}},
		{"jwt-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}

	gotPositions := make([]string, 0, len(preparer.calls))
	for _, call := range preparer.calls {
		gotPositions = append(gotPositions, call.Position)
	}
	if !reflect.DeepEqual(gotPositions, wantPositions) {
		t.Fatalf("Prepare() positions = %#v, want %#v", gotPositions, wantPositions)
	}
	if len(p.auths) != 3 {
		t.Fatalf("published auths = %d, want 3", len(p.auths))
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want all plugins in an entry to be accepted", err)
	}
	p.Stop()
	p.Stop()
	wantClosed := []string{wantPositions[2], wantPositions[1], wantPositions[0]}
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("Close() order = %#v, want %#v", closed, wantClosed)
	}
}

func TestValidatePreMaterializationRejectsDisabledAndUnsupportedAuthPlugins(t *testing.T) {
	tests := []struct {
		name    string
		plugins []AuthPluginConfig
		enabled func(string) bool
		want    string
	}{
		{
			name: "disabled",
			plugins: []AuthPluginConfig{
				{"unknown-auth": {}},
				{"key-auth": {}},
			},
			enabled: func(name string) bool { return name != "unknown-auth" },
			want:    "disabled",
		},
		{
			name: "unsupported",
			plugins: []AuthPluginConfig{
				{"key-auth": {}},
				{"unknown-auth": {}},
			},
			enabled: func(string) bool { return true },
			want:    "unknown-auth",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: Config{AuthPlugins: test.plugins}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			p.SetPluginEnabledChecker(test.enabled)

			err := p.ValidatePreMaterialization()
			if err == nil || !strings.Contains(err.Error(), "unknown-auth") ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidatePreMaterialization() error = %v, want %s rejection", err, test.want)
			}
			if len(p.auths) != 0 || p.current != nil {
				t.Fatalf("rejection published child state: auths=%d current=%v", len(p.auths), p.current)
			}
		})
	}
}

func TestMaterializeScopedSecretsThirdFailureStopsEarlierChildrenReverseOnce(t *testing.T) {
	closed := []string{}
	preparer := &scriptedChildPreparer{failAt: 3, closed: &closed, children: map[string]authPlugin{}}
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
		"multi-auth/entry/2/factory/jwt-auth",
	}
	for _, position := range positions {
		preparer.children[position] = &scriptedAuthPlugin{config: map[string]any{}}
	}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
		{"jwt-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	if err == nil {
		t.Fatal("MaterializeScopedSecrets() error = nil, want third-child failure")
	}
	if got := err.Error(); got == "" || got == "poison raw credential" {
		t.Fatalf("MaterializeScopedSecrets() error = %q, want fixed redacted error", got)
	}
	wantClosed := []string{positions[2], positions[1], positions[0]}
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("Close() order = %#v, want %#v", closed, wantClosed)
	}
	p.Stop()
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("Close() order after Stop = %#v, want unchanged %#v", closed, wantClosed)
	}
	if len(p.auths) != 0 {
		t.Fatalf("published auths = %d, want zero", len(p.auths))
	}
}

func TestPostInitRequiresCompletedPreparation(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want missing preparation to fail closed")
	}
	if len(p.auths) != 0 {
		t.Fatalf("PostInit() constructed %d auths, want zero", len(p.auths))
	}
}

func TestMaterializeScopedSecretsRequiresPreparerAndPreservesCancellation(t *testing.T) {
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); !errors.Is(
		err, errAuthChildPreparation,
	) {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want fixed preparer error", err)
	}
	if len(p.auths) != 0 {
		t.Fatalf("published auths = %d, want zero", len(p.auths))
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.MaterializeScopedSecrets(canceled, base.ScopedSecretAccess{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("MaterializeScopedSecrets(canceled) error = %v, want context.Canceled", err)
	}
}

func TestStopIsConcurrentAndIdempotent(t *testing.T) {
	closed := []string{}
	preparer := &scriptedChildPreparer{closed: &closed, children: map[string]authPlugin{}}
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	for _, position := range positions {
		preparer.children[position] = &scriptedAuthPlugin{config: map[string]any{}}
	}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			p.Stop()
		})
	}
	wait.Wait()
	want := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(closed, want) {
		t.Fatalf("concurrent Close() order = %#v, want %#v", closed, want)
	}
}

func TestStopReturnsButDefersCloseUntilActiveAuthChildFinishes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	closed := []string{}
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	preparer := &scriptedChildPreparer{closed: &closed, children: map[string]authPlugin{
		positions[0]: &blockingAuthPlugin{config: map[string]any{}, entered: entered, release: release},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{}},
	}}
	p := newScopedLifecyclePlugin(t, preparer)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		p.RunRequestPhase(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		p.Stop()
	}()

	select {
	case <-stopDone:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Stop() did not return while an auth child request was active")
	}
	if len(closed) != 0 {
		close(release)
		t.Fatal("Stop() closed children before the active request released its lease")
	}
	close(release)
	<-requestDone
	wantClosed := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(closed, wantClosed) {
		t.Fatalf("Close() order = %#v, want %#v", closed, wantClosed)
	}
}

func TestRematerializePublishesNewButDefersOldCloseUntilActiveAuthChildFinishes(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	oldClosed := []string{}
	newClosed := []string{}
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	preparer := &scriptedChildPreparer{closed: &oldClosed, children: map[string]authPlugin{
		positions[0]: &blockingAuthPlugin{
			config:  map[string]any{"generation": "old"},
			entered: entered,
			release: release,
		},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "old"}},
	}}
	p := newScopedLifecyclePlugin(t, preparer)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		p.RunRequestPhase(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered
	preparer.replaceChildren(map[string]authPlugin{
		positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
	}, &newClosed)
	rematerializeDone := make(chan error, 1)
	go func() {
		rematerializeDone <- p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	}()

	select {
	case err := <-rematerializeDone:
		if err != nil {
			close(release)
			t.Fatalf("MaterializeScopedSecrets() error = %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("rematerialization did not return while an old auth child request was active")
	}
	if len(oldClosed) != 0 {
		close(release)
		t.Fatal("rematerialization closed old children before the active request released its lease")
	}
	public := p.Config().(*Config)
	if got := public.AuthPlugins[0]["basic-auth"]["generation"]; got != "new" {
		close(release)
		t.Fatalf("published child generation = %v, want new", got)
	}
	close(release)
	<-requestDone
	wantOldClosed := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(oldClosed, wantOldClosed) {
		t.Fatalf("old Close() order = %#v, want %#v", oldClosed, wantOldClosed)
	}
	p.Stop()
	wantNewClosed := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(newClosed, wantNewClosed) {
		t.Fatalf("new Close() order = %#v, want %#v", newClosed, wantNewClosed)
	}
}

func newScopedLifecyclePlugin(t *testing.T, preparer base.CompositeChildPreparer) *Plugin {
	t.Helper()
	p := newUnpreparedScopedLifecyclePlugin(t, preparer)
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}

func newUnpreparedScopedLifecyclePlugin(t *testing.T, preparer base.CompositeChildPreparer) *Plugin {
	t.Helper()
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	return p
}

func TestPrepareCallbackCanReenterLifecycleWithoutDeadlock(t *testing.T) {
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	closed := []string{}
	preparer := &scriptedChildPreparer{closed: &closed, children: map[string]authPlugin{
		positions[0]: &scriptedAuthPlugin{config: map[string]any{}},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{}},
	}}
	p := newUnpreparedScopedLifecyclePlugin(t, preparer)
	preparer.prepare = func(base.CompositeChildSpec) {
		p.Stop()
		_ = p.PostInit()
	}

	err := runLifecycleWithTimeout(func() error {
		return p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
	})
	if errors.Is(err, errLifecycleTimeout) {
		t.Fatal("Prepare callback reentry deadlocked outer lifecycle")
	}
	if err == nil {
		t.Fatal("stale preparation published after callback Stop invalidated its epoch")
	}
	if len(closed) != 1 {
		t.Fatalf("stale prepared children closed = %d, want the acquired child closed once", len(closed))
	}
}

func TestOwnerCloseCanReenterLifecycleWithoutDeadlock(t *testing.T) {
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	closed := []string{}
	preparer := &scriptedChildPreparer{closed: &closed, children: map[string]authPlugin{
		positions[0]: &scriptedAuthPlugin{config: map[string]any{}},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{}},
	}}
	p := newScopedLifecyclePlugin(t, preparer)
	preparer.close = func() {
		p.Stop()
		_ = p.PostInit()
	}
	// The close hook is captured when an owner is prepared, so rematerialize
	// once before exercising Stop.
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("MaterializeScopedSecrets() error = %v", err)
	}

	err := runLifecycleWithTimeout(func() error {
		p.Stop()
		return nil
	})
	if errors.Is(err, errLifecycleTimeout) {
		t.Fatal("owner Close callback reentry deadlocked outer lifecycle")
	}
	if len(closed) != 4 {
		t.Fatalf("closed owners = %d, want two retired generations closed exactly once", len(closed))
	}
}

func TestAuthChildCanReenterOuterLifecycleWithoutDeadlock(t *testing.T) {
	tests := []struct {
		name  string
		phase bool
		run   func(*Plugin) error
	}{
		{name: "request-phase-stop", phase: true, run: func(p *Plugin) error { p.Stop(); return nil }},
		{name: "request-phase-post-init", phase: true, run: func(p *Plugin) error { return p.PostInit() }},
		{name: "request-phase-rematerialize", phase: true, run: func(p *Plugin) error {
			return p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
		}},
		{name: "handler-stop", run: func(p *Plugin) error { p.Stop(); return nil }},
		{name: "handler-post-init", run: func(p *Plugin) error { return p.PostInit() }},
		{name: "handler-rematerialize", run: func(p *Plugin) error {
			return p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldClosed := []string{}
			newClosed := []string{}
			positions := []string{
				"multi-auth/entry/0/factory/basic-auth",
				"multi-auth/entry/1/factory/key-auth",
			}
			var p *Plugin
			var callbackErr error
			var closeDuringRequest bool
			preparer := &scriptedChildPreparer{closed: &oldClosed, children: map[string]authPlugin{}}
			reenter := func() {
				callbackErr = test.run(p)
				closeDuringRequest = len(oldClosed) != 0
			}
			if test.phase {
				entered := make(chan struct{})
				release := make(chan struct{})
				close(release)
				preparer.children[positions[0]] = &blockingAuthPlugin{
					config: map[string]any{}, entered: entered, release: release, run: reenter,
				}
			} else {
				preparer.children[positions[0]] = &reentrantHandlerAuthPlugin{
					config: map[string]any{}, run: reenter,
				}
			}
			preparer.children[positions[1]] = &scriptedAuthPlugin{config: map[string]any{}}
			p = newScopedLifecyclePlugin(t, preparer)
			if strings.HasSuffix(test.name, "rematerialize") {
				preparer.replaceChildren(map[string]authPlugin{
					positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
					positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
				}, &newClosed)
			}

			err := runLifecycleWithTimeout(func() error {
				p.RunRequestPhase(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
				return nil
			})
			if errors.Is(err, errLifecycleTimeout) {
				t.Fatal("auth child callback reentry deadlocked outer lifecycle")
			}
			if callbackErr != nil {
				t.Fatalf("reentrant lifecycle error = %v", callbackErr)
			}
			if closeDuringRequest {
				t.Fatal("retired owners closed before the active request released its lease")
			}
			if strings.HasSuffix(test.name, "stop") || strings.HasSuffix(test.name, "rematerialize") {
				wantOld := []string{positions[1], positions[0]}
				if !reflect.DeepEqual(oldClosed, wantOld) {
					t.Fatalf("old Close() order = %#v, want %#v", oldClosed, wantOld)
				}
			}
			p.Stop()
			if strings.HasSuffix(test.name, "post-init") {
				wantOld := []string{positions[1], positions[0]}
				if !reflect.DeepEqual(oldClosed, wantOld) {
					t.Fatalf("Close() order = %#v, want %#v", oldClosed, wantOld)
				}
			}
			if strings.HasSuffix(test.name, "rematerialize") {
				wantNew := []string{positions[1], positions[0]}
				if !reflect.DeepEqual(newClosed, wantNew) {
					t.Fatalf("new Close() order = %#v, want %#v", newClosed, wantNew)
				}
			}
		})
	}
}

type resolvingConfigPreparer struct {
	mu           sync.Mutex
	seen         []map[string]any
	resolveCalls int
}

func (p *resolvingConfigPreparer) Prepare(
	_ context.Context,
	_ base.ScopedSecretAccess,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	p.mu.Lock()
	p.seen = append(p.seen, maps.Clone(spec.Config))
	config := maps.Clone(spec.Config)
	if raw, ok := config["realm"].(string); ok && raw == "$ENV://MULTI_AUTH_RAW_SOURCE" {
		p.resolveCalls++
		config["realm"] = "plugin_config#sha256:descriptor"
	}
	if spec.Factory == "key-auth" {
		config["header"] = "defaulted-header"
	}
	p.mu.Unlock()
	return &scriptedPreparedChild{
		factory:  spec.Factory,
		instance: &scriptedAuthPlugin{config: config},
	}, nil
}

func TestRematerializeUsesImmutableRawConfigAndPublishesOnlyDescriptors(t *testing.T) {
	preparer := &resolvingConfigPreparer{}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {"realm": "$ENV://MULTI_AUTH_RAW_SOURCE"}},
		{"key-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
			t.Fatalf("MaterializeScopedSecrets() error = %v", err)
		}
	}
	if preparer.resolveCalls != 2 {
		t.Fatalf("resolver calls = %d, want 2", preparer.resolveCalls)
	}
	if len(preparer.seen) != 4 {
		t.Fatalf("Prepare() calls = %d, want 4", len(preparer.seen))
	}
	for _, index := range []int{0, 2} {
		if got := preparer.seen[index]["realm"]; got != "$ENV://MULTI_AUTH_RAW_SOURCE" {
			t.Fatalf("Prepare()[%d] realm = %v, want immutable raw source", index, got)
		}
	}
	for _, index := range []int{1, 3} {
		if _, exists := preparer.seen[index]["header"]; exists {
			t.Fatalf("Prepare()[%d] received defaulted public config as raw source", index)
		}
	}
	public, ok := p.Config().(*Config)
	if !ok {
		t.Fatalf("Config() = %T, want *Config", p.Config())
	}
	if got := public.AuthPlugins[0]["basic-auth"]["realm"]; got != "plugin_config#sha256:descriptor" {
		t.Fatalf("public realm = %v, want descriptor", got)
	}
	if got := public.AuthPlugins[1]["key-auth"]["header"]; got != "defaulted-header" {
		t.Fatalf("public header = %v, want prepared default", got)
	}
}

type mutatingFailOncePreparer struct {
	calls        int
	nestedValues []string
}

func (p *mutatingFailOncePreparer) Prepare(
	_ context.Context,
	_ base.ScopedSecretAccess,
	spec base.CompositeChildSpec,
) (base.PreparedCompositeChild, error) {
	p.calls++
	config := spec.Config
	if spec.Factory == "basic-auth" {
		nested := config["nested"].([]any)
		object := nested[0].(map[string]any)
		p.nestedValues = append(p.nestedValues, object["value"].(string))
		if p.calls == 1 {
			object["value"] = "mutated-by-failed-prepare"
			return nil, errors.New("injected preparation failure")
		}
	}
	return &scriptedPreparedChild{
		factory:  spec.Factory,
		instance: &scriptedAuthPlugin{config: config},
	}, nil
}

func TestFailedPrepareCannotMutateNestedImmutableRawSource(t *testing.T) {
	preparer := &mutatingFailOncePreparer{}
	p := &Plugin{config: Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {
			"realm":  "raw-realm",
			"nested": []any{map[string]any{"value": "original-nested-value"}},
		}},
		{"key-auth": {}},
	}}}
	p.SetDependencies(base.Dependencies{CompositeChildren: preparer})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err == nil {
		t.Fatal("first MaterializeScopedSecrets() error = nil, want injected failure")
	}
	failedPublic := p.Config().(*Config)
	failedNested := failedPublic.AuthPlugins[0]["basic-auth"]["nested"].([]any)[0].(map[string]any)
	if got := failedNested["value"]; got != "original-nested-value" {
		t.Fatalf("public config after failed prepare = %v, want original nested value", got)
	}
	if err := p.MaterializeScopedSecrets(context.Background(), base.ScopedSecretAccess{}); err != nil {
		t.Fatalf("retry MaterializeScopedSecrets() error = %v", err)
	}
	if !reflect.DeepEqual(preparer.nestedValues, []string{"original-nested-value", "original-nested-value"}) {
		t.Fatalf("Prepare() nested values = %#v, want original value on both attempts", preparer.nestedValues)
	}
	public := p.Config().(*Config)
	publicNested := public.AuthPlugins[0]["basic-auth"]["nested"].([]any)[0].(map[string]any)
	if got := publicNested["value"]; got != "original-nested-value" {
		t.Fatalf("published public config = %v, want unpolluted nested value", got)
	}
}

func TestCancellationAfterLastPreparedChildRejectsCommitAndKeepsOldGeneration(t *testing.T) {
	positions := []string{
		"multi-auth/entry/0/factory/basic-auth",
		"multi-auth/entry/1/factory/key-auth",
	}
	oldClosed := []string{}
	newClosed := []string{}
	preparer := &scriptedChildPreparer{closed: &oldClosed, children: map[string]authPlugin{
		positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "old"}},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "old"}},
	}}
	p := newScopedLifecyclePlugin(t, preparer)
	p.stateMu.Lock()
	oldGeneration := p.current
	p.stateMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	preparer.replaceChildren(map[string]authPlugin{
		positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
		positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "new"}},
	}, &newClosed)
	preparer.mu.Lock()
	preparer.prepare = func(spec base.CompositeChildSpec) {
		if spec.Position == positions[1] {
			cancel()
		}
	}
	preparer.mu.Unlock()

	err := p.MaterializeScopedSecrets(ctx, base.ScopedSecretAccess{})
	p.stateMu.Lock()
	current := p.current
	p.stateMu.Unlock()
	oldClosedBeforeCleanup := append([]string(nil), oldClosed...)
	newClosedBeforeCleanup := append([]string(nil), newClosed...)
	p.Stop()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("MaterializeScopedSecrets() error = %v, want context.Canceled", err)
	}
	if current != oldGeneration {
		t.Fatal("canceled preparation replaced the old generation")
	}
	if len(oldClosedBeforeCleanup) != 0 {
		t.Fatalf("old owners closed before explicit cleanup = %#v, want retained", oldClosedBeforeCleanup)
	}
	wantNewClosed := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(newClosedBeforeCleanup, wantNewClosed) {
		t.Fatalf("staged Close() order = %#v, want %#v", newClosedBeforeCleanup, wantNewClosed)
	}
	wantOldClosed := []string{positions[1], positions[0]}
	if !reflect.DeepEqual(oldClosed, wantOldClosed) {
		t.Fatalf("old cleanup Close() order = %#v, want %#v", oldClosed, wantOldClosed)
	}
}

type deterministicCommitContext struct {
	context.Context
	mu  sync.Mutex
	err error
}

func (ctx *deterministicCommitContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	return ctx.err
}

func (ctx *deterministicCommitContext) setError(err error) {
	ctx.mu.Lock()
	ctx.err = err
	ctx.mu.Unlock()
}

func TestCommitCancellationWinsOverStalePreparationEpoch(t *testing.T) {
	for _, wantErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			positions := []string{
				"multi-auth/entry/0/factory/basic-auth",
				"multi-auth/entry/1/factory/key-auth",
			}
			oldClosed := []string{}
			stagedClosed := []string{}
			preparer := &scriptedChildPreparer{closed: &oldClosed, children: map[string]authPlugin{
				positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "old"}},
				positions[1]: &scriptedAuthPlugin{config: map[string]any{"generation": "old"}},
			}}
			p := newScopedLifecyclePlugin(t, preparer)
			p.stateMu.Lock()
			oldGeneration := p.current
			p.stateMu.Unlock()
			commitCtx := &deterministicCommitContext{Context: context.Background()}
			invalidateAtCommit := func() {
				commitCtx.setError(wantErr)
				_, _ = p.beginPreparation()
			}
			preparer.replaceChildren(map[string]authPlugin{
				positions[0]: &scriptedAuthPlugin{config: map[string]any{"generation": "staged"}},
				positions[1]: &callbackConfigAuthPlugin{
					config: map[string]any{"generation": "staged"}, onConfig: invalidateAtCommit,
				},
			}, &stagedClosed)

			err := p.MaterializeScopedSecrets(commitCtx, base.ScopedSecretAccess{})
			p.stateMu.Lock()
			current := p.current
			p.stateMu.Unlock()
			oldClosedBeforeCleanup := append([]string(nil), oldClosed...)
			stagedClosedBeforeCleanup := append([]string(nil), stagedClosed...)
			p.Stop()

			if !errors.Is(err, wantErr) {
				t.Fatalf("MaterializeScopedSecrets() error = %v, want %v", err, wantErr)
			}
			if current != oldGeneration {
				t.Fatal("canceled stale preparation replaced the old generation")
			}
			if len(oldClosedBeforeCleanup) != 0 {
				t.Fatalf("old owners closed before explicit cleanup = %#v", oldClosedBeforeCleanup)
			}
			wantStagedClosed := []string{positions[1], positions[0]}
			if !reflect.DeepEqual(stagedClosedBeforeCleanup, wantStagedClosed) {
				t.Fatalf("staged Close() order = %#v, want %#v", stagedClosedBeforeCleanup, wantStagedClosed)
			}
		})
	}
}

var errLifecycleTimeout = errors.New("lifecycle callback timed out")

func runLifecycleWithTimeout(run func() error) error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Second):
		return errLifecycleTimeout
	}
}
