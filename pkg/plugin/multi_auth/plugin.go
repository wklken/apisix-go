package multi_auth

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	config         Config
	auths          []configuredAuth
	enabledChecker func(string) bool
}

const (
	priority                  = 2600
	name                      = "multi-auth"
	maxFailureDiagnosticBytes = 4 * 1024
)

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
	source   io.ReadCloser
	captured bytes.Buffer
}

type replayReadCloser struct {
	io.Reader
	closer io.Closer
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
	if len(p.config.AuthPlugins) < 2 {
		return fmt.Errorf("auth_plugins must contain at least two auth plugins")
	}

	p.auths = make([]configuredAuth, 0, len(p.config.AuthPlugins))
	for _, authPlugin := range p.config.AuthPlugins {
		if len(authPlugin) != 1 {
			return fmt.Errorf("each auth_plugins entry must contain exactly one auth plugin")
		}
		for authName, authConfig := range authPlugin {
			if p.enabledChecker != nil && !p.enabledChecker(authName) {
				return fmt.Errorf("multi-auth child plugin %q is disabled", authName)
			}
			auth, err := newAuthPlugin(authName)
			if err != nil {
				return err
			}
			if err := auth.Init(); err != nil {
				return err
			}
			compiledSchema, err := util.CompileSchema(auth.GetSchema())
			if err != nil {
				return fmt.Errorf("plugin %s check schema failed: %w", authName, err)
			}
			if err := compiledSchema.Validate(authConfig); err != nil {
				return fmt.Errorf("plugin %s check schema failed: %w", authName, err)
			}
			if err := util.Parse(authConfig, auth.Config()); err != nil {
				return fmt.Errorf("plugin %s parse config failed: %w", authName, err)
			}
			if err := auth.PostInit(); err != nil {
				return err
			}
			p.auths = append(p.auths, configuredAuth{name: authName, plugin: auth})
		}
	}
	if len(p.auths) < 2 {
		return fmt.Errorf("auth_plugins must contain at least two auth plugins")
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attachLegacyConsumer(r)
		next.ServeHTTP(w, r)
	}))
}

func attachLegacyConsumer(r *http.Request) {
	state, ok := ctx.AuthenticationStateFrom(r)
	if !ok {
		return
	}
	consumer := state.Consumer()
	ctx.RegisterApisixVar(r, "$consumer", consumer)
	ctx.RegisterApisixVar(r, "$consumer_name", consumer.Username)
	ctx.RegisterApisixVar(r, "$consumer_group_id", consumer.GroupID)
	r.Header.Set("X-Consumer-Username", consumer.Username)
}

func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	failures := make([]authFailure, 0, len(p.auths))
	for _, auth := range p.auths {
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
		if result.Decision == base.RequestContinue {
			return a.successRequest(authenticatedRequest, r, originalBody, bodyState), authFailure{}
		}
		authenticatedRequest = nil
	} else {
		a.plugin.Handler(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			authenticatedRequest = request
		})).ServeHTTP(writer, probeRequest)
		if authenticatedRequest != nil {
			return a.successRequest(authenticatedRequest, r, originalBody, bodyState), authFailure{}
		}
	}
	if authenticatedRequest != nil {
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
	if limit < math.MaxInt64 {
		limit++
	}
	state := &probeBodyState{source: original.Body}
	probe.Body = io.NopCloser(io.TeeReader(io.LimitReader(state.source, limit), &state.captured))
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
	request.Body = &replayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(s.captured.Bytes()), s.source),
		closer: s.source,
	}
}

func (s *probeBodyState) attachWinnerBody(request *http.Request) {
	request.Body = &replayReadCloser{
		Reader: io.MultiReader(bytes.NewReader(s.captured.Bytes()), s.source),
		closer: s.source,
	}
}

func (r *replayReadCloser) Close() error {
	return r.closer.Close()
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
