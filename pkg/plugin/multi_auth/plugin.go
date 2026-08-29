package multi_auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/hmac_auth"
	"github.com/wklken/apisix-go/pkg/plugin/jwe_decrypt"
	"github.com/wklken/apisix-go/pkg/plugin/jwt_auth"
	"github.com/wklken/apisix-go/pkg/plugin/key_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ldap_auth"
	"github.com/wklken/apisix-go/pkg/plugin/wolf_rbac"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config          Config
	auths           []configuredAuth
	enabledChecker  func(string) bool
	stateMu         sync.Mutex
	stateEpoch      uint64
	current         *authGeneration
	children        []base.PreparedCompositeChild
	sourceConfig    Config
	sourceConfigSet bool
	publicConfig    atomic.Pointer[Config]
}

type authGeneration struct {
	auths    []configuredAuth
	children []base.PreparedCompositeChild
	active   int
	retired  bool
	closed   bool
}

const (
	priority                  = 2600
	name                      = "multi-auth"
	maxFailureDiagnosticBytes = 4 * 1024
)

var errAuthChildPreparation = errors.New("multi-auth child preparation failed")

const schema = `
{
  "type": "object",
  "title": "work with route or service object",
  "properties": {
    "auth_plugins": {
      "type": "array",
      "minItems": 2
    }
  },
  "required": ["auth_plugins"]
}
`

type Config struct {
	AuthPlugins []AuthPluginConfig `json:"auth_plugins"`
}

type AuthPluginConfig map[string]map[string]any

type authPlugin interface {
	Init() error
	PostInit() error
	Config() any
	GetSchema() string
	Handler(http.Handler) http.Handler
}

// requestBodyIsolation is implemented by auth plugin configs that consume
// the request body while authenticating. multi-auth isolates and replays the
// body for every plugin that advertises body consumption, instead of
// hard-coding a specific plugin type.
type requestBodyIsolation interface {
	BodyIsolation() (enabled bool, max int64)
}

type requestBodyIsolationTempDir interface {
	BodyIsolationTempDir() string
}

type configuredAuth struct {
	name   string
	plugin authPlugin
}

type probeResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

type authFailure struct {
	name    string
	status  int
	message string
}

type probeBodyState struct {
	snapshot *base.RequestBodySnapshot
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) SetPluginEnabledChecker(checker func(string) bool) {
	p.enabledChecker = checker
}

func (p *Plugin) PostInit() error {
	p.stateMu.Lock()
	current := p.current
	p.stateMu.Unlock()
	if current == nil || len(current.auths) < 2 {
		return errors.New("auth_plugins must be prepared before PostInit")
	}
	return nil
}

type authChildSpec struct {
	entry    int
	factory  string
	config   map[string]any
	position string
}

func authChildPosition(entry int, factory string) string {
	return "multi-auth/entry/" + strconv.Itoa(entry) + "/factory/" + factory
}

func (p *Plugin) ValidatePreMaterialization() error {
	_, err := p.validatedAuthChildSpecs(p.rawSourceConfig())
	return err
}

func (p *Plugin) validatedAuthChildSpecs(source Config) ([]authChildSpec, error) {
	specs := make([]authChildSpec, 0, len(source.AuthPlugins))
	for entry, configured := range source.AuthPlugins {
		factories := make([]string, 0, len(configured))
		for factory := range configured {
			factories = append(factories, factory)
		}
		sort.Strings(factories)
		for _, factory := range factories {
			if p.enabledChecker != nil && !p.enabledChecker(factory) {
				return nil, fmt.Errorf("multi-auth child plugin %q is disabled", factory)
			}
			config := configured[factory]
			if config == nil {
				return nil, fmt.Errorf("multi-auth child plugin %q has invalid config", factory)
			}
			if err := validateRawAuthChild(factory, config); err != nil {
				return nil, err
			}
			specs = append(specs, authChildSpec{
				entry: entry, factory: factory, config: config,
				position: authChildPosition(entry, factory),
			})
		}
	}
	if len(specs) < 2 {
		return nil, errors.New("auth_plugins must contain at least two auth plugins")
	}
	return specs, nil
}

func validateRawAuthChild(factory string, config map[string]any) error {
	child, err := newAuthPlugin(factory)
	if err != nil {
		return fmt.Errorf("multi-auth child plugin %q is not supported", factory)
	}
	defer stopAuthChild(child)
	if err := child.Init(); err != nil {
		return fmt.Errorf("multi-auth child plugin %q validation failed", factory)
	}
	compiledSchema, err := util.CompileSchema(child.GetSchema())
	if err != nil {
		return fmt.Errorf("multi-auth child plugin %q validation failed", factory)
	}
	if err := compiledSchema.Validate(config); err != nil {
		return fmt.Errorf("multi-auth child plugin %q has invalid config", factory)
	}
	return nil
}

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	if ctx == nil {
		return errAuthChildPreparation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source, epoch := p.beginPreparation()
	specs, err := p.validatedAuthChildSpecs(source)
	if err != nil {
		return err
	}
	preparer := p.CompositeChildPreparer()
	if preparer == nil {
		return errAuthChildPreparation
	}
	generation, publicConfig, err := p.prepareAuthChildren(ctx, access, epoch, source, specs, func(
		ctx context.Context,
		access base.ScopedSecretAccess,
		spec authChildSpec,
	) (base.PreparedCompositeChild, error) {
		return preparer.Prepare(ctx, access, base.CompositeChildSpec{
			Factory: spec.factory, Config: spec.config, Position: spec.position,
		})
	})
	if err != nil {
		return err
	}
	return p.publishPreparedGeneration(ctx, epoch, generation, publicConfig)
}

type authChildPrepareFunc func(
	context.Context,
	base.ScopedSecretAccess,
	authChildSpec,
) (base.PreparedCompositeChild, error)

func (p *Plugin) prepareAuthChildren(
	ctx context.Context,
	access base.ScopedSecretAccess,
	epoch uint64,
	source Config,
	specs []authChildSpec,
	prepare authChildPrepareFunc,
) (*authGeneration, Config, error) {
	stagedConfig := cloneAuthPluginConfigs(source.AuthPlugins)
	stagedAuths := make([]configuredAuth, 0, len(specs))
	stagedOwners := make([]base.PreparedCompositeChild, 0, len(specs))
	fail := func(failing base.PreparedCompositeChild, err error) (*authGeneration, Config, error) {
		if failing != nil {
			failing.Close()
		}
		closePreparedAuthChildren(stagedOwners)
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, Config{}, contextErr
		}
		if errors.Is(err, context.Canceled) {
			return nil, Config{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, Config{}, context.DeadlineExceeded
		}
		return nil, Config{}, errAuthChildPreparation
	}

	for _, spec := range specs {
		owner, err := prepare(ctx, access, spec)
		if err != nil {
			return fail(owner, err)
		}
		if !p.preparationCurrent(epoch) {
			return fail(owner, errAuthChildPreparation)
		}
		if owner == nil || owner.Factory() != spec.factory {
			return fail(owner, errAuthChildPreparation)
		}
		child, ok := owner.Instance().(authPlugin)
		if !ok || child == nil {
			return fail(owner, errAuthChildPreparation)
		}
		publicConfig, err := authChildPublicConfig(child.Config())
		if err != nil {
			return fail(owner, err)
		}
		stagedOwners = append(stagedOwners, owner)
		stagedAuths = append(stagedAuths, configuredAuth{name: spec.factory, plugin: child})
		stagedConfig[spec.entry][spec.factory] = publicConfig
	}

	return &authGeneration{auths: stagedAuths, children: stagedOwners}, Config{
		AuthPlugins: stagedConfig,
	}, nil
}

func stopAuthChild(child authPlugin) {
	if stopper, ok := child.(interface{ Stop() }); ok {
		stopper.Stop()
	}
}

func authChildPublicConfig(config any) (map[string]any, error) {
	body, err := json.Marshal(config)
	if err != nil {
		return nil, errAuthChildPreparation
	}
	var public map[string]any
	if err := json.Unmarshal(body, &public); err != nil || public == nil {
		return nil, errAuthChildPreparation
	}
	return public, nil
}

func cloneAuthPluginConfigs(configs []AuthPluginConfig) []AuthPluginConfig {
	cloned := make([]AuthPluginConfig, len(configs))
	for entry, configured := range configs {
		cloned[entry] = make(AuthPluginConfig, len(configured))
		for factory, config := range configured {
			cloned[entry][factory] = cloneAuthPluginConfig(config)
		}
	}
	return cloned
}

func cloneAuthPluginConfig(config map[string]any) map[string]any {
	if config == nil {
		return nil
	}
	cloned := make(map[string]any, len(config))
	for key, value := range config {
		cloned[key] = cloneAuthPluginConfigValue(value)
	}
	return cloned
}

func cloneAuthPluginConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAuthPluginConfig(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAuthPluginConfigValue(item)
		}
		return cloned
	default:
		return value
	}
}

func cloneMultiAuthConfig(config Config) Config {
	return Config{AuthPlugins: cloneAuthPluginConfigs(config.AuthPlugins)}
}

func (p *Plugin) rawSourceConfig() Config {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.sourceConfigSet {
		return cloneMultiAuthConfig(p.sourceConfig)
	}
	return cloneMultiAuthConfig(p.config)
}

func (p *Plugin) beginPreparation() (Config, uint64) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if !p.sourceConfigSet {
		p.sourceConfig = cloneMultiAuthConfig(p.config)
		p.sourceConfigSet = true
	}
	p.stateEpoch++
	return cloneMultiAuthConfig(p.sourceConfig), p.stateEpoch
}

func (p *Plugin) preparationCurrent(epoch uint64) bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.stateEpoch == epoch
}

func (p *Plugin) publishPreparedGeneration(
	ctx context.Context,
	epoch uint64,
	generation *authGeneration,
	publicConfig Config,
) error {
	p.stateMu.Lock()
	if err := ctx.Err(); err != nil {
		p.stateMu.Unlock()
		closePreparedAuthChildren(generation.children)
		return err
	}
	if p.stateEpoch != epoch {
		p.stateMu.Unlock()
		closePreparedAuthChildren(generation.children)
		return errAuthChildPreparation
	}
	previous := p.current
	p.current = generation
	p.auths = generation.auths
	p.children = generation.children
	public := cloneMultiAuthConfig(publicConfig)
	p.publicConfig.Store(&public)
	retired := retireAuthGenerationLocked(previous)
	p.stateMu.Unlock()
	closePreparedAuthChildren(retired)
	return nil
}

func retireAuthGenerationLocked(generation *authGeneration) []base.PreparedCompositeChild {
	if generation == nil {
		return nil
	}
	generation.retired = true
	return closableAuthGenerationLocked(generation)
}

func closableAuthGenerationLocked(generation *authGeneration) []base.PreparedCompositeChild {
	if generation == nil || !generation.retired || generation.active != 0 || generation.closed {
		return nil
	}
	generation.closed = true
	children := generation.children
	generation.children = nil
	return children
}

func closePreparedAuthChildren(children []base.PreparedCompositeChild) {
	for _, child := range slices.Backward(children) {
		child.Close()
	}
}

func (p *Plugin) Stop() {
	p.stateMu.Lock()
	p.stateEpoch++
	current := p.current
	p.current = nil
	p.children = nil
	p.auths = nil
	retired := retireAuthGenerationLocked(current)
	p.stateMu.Unlock()
	closePreparedAuthChildren(retired)
}

func (p *Plugin) Config() any {
	if public := p.publicConfig.Load(); public != nil {
		return public
	}
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.AttachConsumerFromAuthenticationState(r)
		next.ServeHTTP(w, r)
	}))
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	generation := p.acquireAuthGeneration()
	if generation != nil {
		defer p.releaseAuthGeneration(generation)
	}
	auths := []configuredAuth(nil)
	if generation != nil {
		auths = generation.auths
	}
	failures := make([]authFailure, 0, len(auths))
	for _, auth := range auths {
		authenticatedRequest, failure := auth.succeeds(r)
		if authenticatedRequest != nil {
			return base.ContinueRequest(authenticatedRequest)
		}
		failures = append(failures, failure)
	}
	for _, failure := range failures {
		failure.log()
	}

	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"message":"Authorization Failed"}`))
	return base.StopRequest(r)
}

func (p *Plugin) acquireAuthGeneration() *authGeneration {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	generation := p.current
	if generation != nil {
		generation.active++
	}
	return generation
}

func (p *Plugin) releaseAuthGeneration(generation *authGeneration) {
	p.stateMu.Lock()
	if generation.active > 0 {
		generation.active--
	}
	retired := closableAuthGenerationLocked(generation)
	p.stateMu.Unlock()
	closePreparedAuthChildren(retired)
}

func (a configuredAuth) succeeds(r *http.Request) (*http.Request, authFailure) {
	var authenticatedRequest *http.Request
	originalBody := r.Body
	apisixVars := cloneContextMap(ctx.GetApisixVars(r))
	requestVars := cloneContextMap(ctx.GetRequestVars(r))
	writer := &probeResponseWriter{header: http.Header{}, status: http.StatusOK}
	probeTemplate := r.Clone(r.Context())
	probeTemplate.Body = nil
	probeTemplate.GetBody = nil
	probeRequest := ctx.NewAuthenticationProbeRequest(probeTemplate)
	bodyState := a.isolateRequestBody(r, probeRequest)
	var recordedDiagnostic bytes.Buffer
	probeRequest = ctx.WithAuthProbeDiagnosticRecorder(probeRequest, func(message string) {
		appendFailureDiagnostic(&recordedDiagnostic, message)
	})
	if phase, ok := a.plugin.(base.RequestPhasePlugin); ok {
		result := phase.RunRequestPhase(writer, probeRequest)
		if result.Request != nil {
			authenticatedRequest = result.Request
		} else {
			authenticatedRequest = probeRequest
		}
		if result.Decision == base.RequestContinue && hasAuthenticationState(authenticatedRequest) {
			return a.successRequest(authenticatedRequest, r, originalBody, bodyState), authFailure{}
		}
		authenticatedRequest = nil
	} else {
		a.plugin.Handler(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			authenticatedRequest = request
		})).ServeHTTP(writer, probeRequest)
		if hasAuthenticationState(authenticatedRequest) {
			return a.successRequest(authenticatedRequest, r, originalBody, bodyState), authFailure{}
		}
		authenticatedRequest = nil
	}
	if hasAuthenticationState(authenticatedRequest) {
		return a.successRequest(authenticatedRequest, r, originalBody, bodyState), authFailure{}
	}
	if bodyState != nil {
		bodyState.restore(r)
	}
	restoreContextMap(ctx.GetApisixVars(r), apisixVars)
	restoreContextMap(ctx.GetRequestVars(r), requestVars)
	message := strings.TrimSpace(recordedDiagnostic.String())
	if message == "" {
		message = strings.TrimSpace(writer.body.String())
	}
	return nil, authFailure{
		name:    a.name,
		status:  writer.status,
		message: message,
	}
}

func (a configuredAuth) successRequest(
	request, original *http.Request,
	originalBody io.ReadCloser,
	bodyState *probeBodyState,
) *http.Request {
	if request == nil {
		request = original
	}
	if bodyState != nil {
		bodyState.attachWinnerBody(request)
	} else if (request.Body == nil || request.Body == http.NoBody) && originalBody != nil {
		request.Body = originalBody
		request.GetBody = original.GetBody
	}
	return ctx.WithAuthProbeDiagnosticRecorder(request, nil)
}

func cloneContextMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	maps.Copy(cloned, source)
	return cloned
}

func restoreContextMap(target, source map[string]any) {
	if target == nil {
		return
	}
	for key := range target {
		if _, ok := source[key]; !ok {
			delete(target, key)
		}
	}
	maps.Copy(target, source)
}

// truncateAuthDiagnostic appends message to buffer without a separator,
// keeping the combined diagnostic within the byte limit.
func truncateAuthDiagnostic(buffer *bytes.Buffer, message string) {
	remaining := maxFailureDiagnosticBytes - buffer.Len()
	if remaining > 0 {
		_, _ = buffer.WriteString(message[:min(len(message), remaining)])
	}
}

func appendFailureDiagnostic(buffer *bytes.Buffer, message string) {
	message = strings.TrimSpace(message)
	if message == "" || buffer.Len() >= maxFailureDiagnosticBytes {
		return
	}
	if buffer.Len() > 0 {
		remaining := maxFailureDiagnosticBytes - buffer.Len()
		separator := "; "
		_, _ = buffer.WriteString(separator[:min(len(separator), remaining)])
	}
	truncateAuthDiagnostic(buffer, message)
}

func (a configuredAuth) isolateRequestBody(original *http.Request, probe *http.Request) *probeBodyState {
	isolator, ok := a.plugin.Config().(requestBodyIsolation)
	if !ok {
		return nil
	}
	enabled, limit := isolator.BodyIsolation()
	if !enabled || original.Body == nil {
		return nil
	}
	tempDir := ""
	if configured, ok := a.plugin.Config().(requestBodyIsolationTempDir); ok {
		tempDir = configured.BodyIsolationTempDir()
	}
	snapshot, err := base.EnsureRequestBodySnapshot(
		original,
		limit,
		base.DefaultRequestBodySnapshotMemoryLimit,
		tempDir,
	)
	state := &probeBodyState{snapshot: snapshot}
	if err != nil {
		probe.Body = &snapshotErrorBody{err: err}
		return state
	}
	if err := base.AttachRequestBodySnapshot(probe, snapshot, false); err != nil {
		probe.Body = &snapshotErrorBody{err: err}
	}
	return state
}

func (f authFailure) log() {
	if f.message == "" {
		logger.Warn(fmt.Sprintf("%s failed to authenticate the request, code: %d", f.name, f.status))
		return
	}
	logger.Warn(fmt.Sprintf("%s failed to authenticate the request, code: %d. error: %s", f.name, f.status, f.message))
}

func (s *probeBodyState) restore(request *http.Request) {
	if s.snapshot != nil {
		_ = base.AttachRequestBodySnapshot(request, s.snapshot, ctx.GetRequestLifecycle(request) == nil)
	}
}

func (s *probeBodyState) attachWinnerBody(request *http.Request) {
	if s.snapshot != nil {
		_ = base.AttachRequestBodySnapshot(request, s.snapshot, ctx.GetRequestLifecycle(request) == nil)
	}
}

type snapshotErrorBody struct {
	err error
}

func (body *snapshotErrorBody) Read([]byte) (int, error) { return 0, body.err }
func (body *snapshotErrorBody) Close() error             { return nil }

func hasAuthenticationState(request *http.Request) bool {
	_, ok := ctx.AuthenticationStateFrom(request)
	return ok
}

func newAuthPlugin(name string) (authPlugin, error) {
	switch name {
	case "basic-auth":
		return &basic_auth.Plugin{}, nil
	case "key-auth":
		return &key_auth.Plugin{}, nil
	case "jwt-auth":
		return &jwt_auth.Plugin{}, nil
	case "hmac-auth":
		return &hmac_auth.Plugin{}, nil
	case "jwe-decrypt":
		return &jwe_decrypt.Plugin{}, nil
	case "ldap-auth":
		return &ldap_auth.Plugin{}, nil
	case "wolf-rbac":
		return &wolf_rbac.Plugin{}, nil
	default:
		return nil, fmt.Errorf("%s plugin is not supported", name)
	}
}

func (w *probeResponseWriter) Header() http.Header {
	return w.header
}

func (w *probeResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func (w *probeResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	truncateAuthDiagnostic(&w.body, string(body))
	return len(body), nil
}

var _ http.ResponseWriter = (*probeResponseWriter)(nil)
