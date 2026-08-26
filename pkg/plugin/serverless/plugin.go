package serverless

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/luautil"
	lua "github.com/yuin/gopher-lua"
	lua_parse "github.com/yuin/gopher-lua/parse"
)

type Plugin struct {
	base.BasePlugin
	config Config

	// compiled holds the immutable precompiled function chunks, parsed once
	// in PostInit instead of on every request.
	compiled         []*lua.FunctionProto
	executionTimeout time.Duration
}

const (
	preFunctionName         = "serverless-pre-function"
	preFunctionPriority     = 10000
	postFunctionName        = "serverless-post-function"
	postFunctionPriority    = -2000
	defaultExecutionTimeout = time.Second
	maxExecutionTimeout     = 10 * time.Second
)

const schema = `
{
  "type": "object",
  "properties": {
    "phase": {
      "type": "string",
      "default": "access",
      "enum": ["rewrite", "access", "header_filter", "body_filter", "log", "before_proxy"]
    },
    "functions": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "minItems": 1
    }
  },
  "required": ["functions"]
}
`

type Config struct {
	Phase     string   `json:"phase,omitempty"`
	Functions []string `json:"functions"`
}

func NewPreFunction() *Plugin {
	return &Plugin{
		BasePlugin: base.BasePlugin{
			Name:     preFunctionName,
			Priority: preFunctionPriority,
		},
	}
}

func NewPostFunction() *Plugin {
	return &Plugin{
		BasePlugin: base.BasePlugin{
			Name:     postFunctionName,
			Priority: postFunctionPriority,
		},
	}
}

func (p *Plugin) Init() error {
	if p.Name == "" {
		p.Name = preFunctionName
	}
	if p.Priority == 0 {
		p.Priority = preFunctionPriority
	}
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	executionTimeout, err := p.configuredExecutionTimeout()
	if err != nil {
		return err
	}
	p.executionTimeout = executionTimeout
	if p.config.Phase == "" {
		p.config.Phase = "access"
	}
	for _, fn := range p.config.Functions {
		proto, err := compileFunction(fn)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.executionTimeout)
		err = validateFunctionProto(ctx, proto)
		cancel()
		if err != nil {
			return err
		}
		p.compiled = append(p.compiled, proto)
	}

	return nil
}

func (p *Plugin) configuredExecutionTimeout() (time.Duration, error) {
	effective := p.StaticConfig()
	if effective == nil {
		return defaultExecutionTimeout, nil
	}
	attributes, ok := effective.Config.PluginAttr[p.Name]
	if !ok {
		return defaultExecutionTimeout, nil
	}
	raw, ok := attributes["execution_timeout_ms"]
	if !ok {
		return defaultExecutionTimeout, nil
	}
	var milliseconds int64
	switch value := raw.(type) {
	case int:
		milliseconds = int64(value)
	case int64:
		milliseconds = value
	case float64:
		if value != math.Trunc(value) {
			return 0, fmt.Errorf("plugin_attr.%s.execution_timeout_ms must be an integer", p.Name)
		}
		milliseconds = int64(value)
	case stdjson.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("plugin_attr.%s.execution_timeout_ms must be an integer", p.Name)
		}
		milliseconds = parsed
	default:
		return 0, fmt.Errorf("plugin_attr.%s.execution_timeout_ms must be an integer", p.Name)
	}
	maxMilliseconds := int64(maxExecutionTimeout / time.Millisecond)
	if milliseconds <= 0 || milliseconds > maxMilliseconds {
		return 0, fmt.Errorf(
			"plugin_attr.%s.execution_timeout_ms must be between 1 and %d",
			p.Name,
			maxMilliseconds,
		)
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// DescribeBindingPhases maps the one configured serverless phase to its
// checked request owner or bounded response callback. The legacy log phase is
// intentionally left for its later owner.
func (p Config) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	phase := p.Phase
	if phase == "" {
		phase = "access"
	}
	switch phase {
	case "rewrite", "access", "before_proxy":
		return base.BindingPhaseDescriptor{RequestStage: phase}, nil
	case "log":
		return base.BindingPhaseDescriptor{RequestStage: "none", Log: true}, nil
	case "header_filter":
		return base.BindingPhaseDescriptor{RequestStage: "none", Header: true}, nil
	case "body_filter":
		return base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true}, nil
	default:
		return base.BindingPhaseDescriptor{}, fmt.Errorf("unsupported serverless phase %q", p.Phase)
	}
}

// RunRequestPhase executes rewrite/access/before_proxy functions without
// invoking downstream request stages itself.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	if !isRequestPhase(p.config.Phase) {
		return base.ContinueRequest(r)
	}
	result, err := p.runFunctions(r, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return base.StopRequest(r)
	}
	if result.respond {
		writeResult(w, result)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	return base.ContinueRequest(r)
}

func (p *Plugin) RunHeaderFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || p.config.Phase != "header_filter" || !p.AppliesToResponseSource(responseSource(r)) {
		return nil
	}
	recorder := responseRecorder(r, state)
	result, err := p.runFunctions(r, recorder)
	if err != nil {
		return err
	}
	applyResponseResult(recorder, result, false)
	copyResponseState(state, recorder)
	return nil
}

func (p *Plugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || p.config.Phase != "body_filter" || !p.AppliesToResponseSource(responseSource(r)) {
		return nil
	}
	recorder := responseRecorder(r, state)
	result, err := p.runFunctions(r, recorder)
	if err != nil {
		return err
	}
	applyResponseResult(recorder, result, true)
	copyResponseState(state, recorder)
	return nil
}

// LogCapturePolicy requests the bounded request/response snapshot that the
// legacy log-phase callback could observe. Other phases do not participate in
// detached log capture and therefore keep the zero policy.
func (p *Plugin) LogCapturePolicy() base.LogCapturePolicy {
	if p.config.Phase != "log" {
		return base.LogCapturePolicy{}
	}
	return base.LogCapturePolicy{
		RequestBodyBytes:  base.MAX_REQ_BODY,
		ResponseBodyBytes: base.MAX_RESP_BODY,
	}
}

// RunLogPhase executes log-phase Lua exactly once against a detached request.
// Any response-like result is reported as a bounded callback error instead of
// being applied to the selected response.
func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if p.config.Phase != "log" {
		return nil
	}
	req, err := detachedRequest(snapshot)
	if err != nil {
		return err
	}
	response := base.NewBufferedResponseWriter()
	for field, values := range snapshot.Response.Header {
		response.Header()[field] = append([]string(nil), values...)
	}
	response.SetStatusCode(snapshot.Outcome.Status)
	response.SetBody(snapshot.Response.Body)
	baselineHeader := response.Header().Clone()
	baselineStatus := response.StatusCode()
	baselineBody := append([]byte(nil), response.Body()...)
	result, err := p.runFunctions(req, response)
	if err != nil {
		return err
	}
	if result.respond || result.bodyModified || result.status != baselineStatus ||
		!reflect.DeepEqual(result.header, baselineHeader) || !bytes.Equal(response.Body(), baselineBody) {
		return fmt.Errorf("serverless log phase attempted response mutation")
	}
	return nil
}

func (p *Plugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	if p.config.Phase != "header_filter" && p.config.Phase != "body_filter" {
		return false
	}
	switch source {
	case apisixctx.ResponseSourceUpstream, apisixctx.ResponseSourceAPISIX, apisixctx.ResponseSourceEarlyStop:
		return true
	default:
		return false
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isRequestPhase(p.config.Phase) {
			result := p.RunRequestPhase(w, r)
			if result.Decision != base.RequestContinue {
				return
			}

			next.ServeHTTP(w, result.Request)
			return
		}
		if p.config.Phase == "log" {
			// The production route invokes RunLogPhase from the detached log
			// executor. Keep direct Handler use response-transparent for legacy
			// callers and compatibility tests.
			next.ServeHTTP(w, r)
			return
		}
		if p.config.Phase != "log" && apisixctx.GetRequestLifecycle(r) != nil {
			next.ServeHTTP(w, r)
			return
		}

		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		result, err := p.runFunctions(r, recorder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result.apply(recorder)
		recorder.Commit(w)
	})
}

func detachedRequest(snapshot base.LogSnapshot) (*http.Request, error) {
	target := snapshot.Request.URL
	if target == "" {
		target = snapshot.Request.URI
	}
	if target == "" {
		target = "/"
	}
	if _, err := url.ParseRequestURI(target); err != nil {
		target = "http://" + snapshot.Request.Host + target
	}
	req, err := http.NewRequest(snapshot.Request.Method, target, bytes.NewReader(snapshot.Request.Body))
	if err != nil {
		return nil, fmt.Errorf("build detached serverless request: %w", err)
	}
	req.Host = snapshot.Request.Host
	req.RemoteAddr = snapshot.Request.RemoteAddr
	req.Header = snapshot.Request.Header.Clone()
	return req, nil
}

func responseRecorder(r *http.Request, state *base.ResponseState) *base.BufferedResponseWriter {
	recorder := base.GetOrCreateTransformResponseWriter(r)
	for field, values := range state.Header {
		recorder.Header()[field] = append([]string(nil), values...)
	}
	recorder.SetStatusCode(state.Status)
	recorder.SetBody(state.Body)
	return recorder
}

func copyResponseState(state *base.ResponseState, recorder *base.BufferedResponseWriter) {
	state.Status = recorder.StatusCode()
	state.Header = recorder.Header().Clone()
	state.Body = append([]byte(nil), recorder.Body()...)
}

func applyResponseResult(recorder *base.BufferedResponseWriter, result luaResult, allowBody bool) {
	if result.status != 0 {
		recorder.SetStatusCode(result.status)
	}
	for field, values := range result.header {
		recorder.Header().Del(field)
		for _, value := range values {
			recorder.Header().Add(field, value)
		}
	}
	if allowBody && result.bodyModified {
		recorder.SetBody(result.body)
		base.InvalidateBodyDerivedHeaders(recorder.Header())
	}
}

func responseSource(r *http.Request) apisixctx.ResponseSource {
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		return lifecycle.ResponseSource()
	}
	if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source != "" {
		return apisixctx.ResponseSource(source)
	}
	return apisixctx.ResponseSourceUnknown
}

func (p *Plugin) runFunctions(r *http.Request, resp *base.BufferedResponseWriter) (luaResult, error) {
	parent := context.Background()
	if r != nil {
		parent = r.Context()
	}
	ctx, cancel := context.WithTimeout(parent, p.executionTimeout)
	defer cancel()
	runner := newLuaRunner(ctx, r, resp)
	defer runner.close()

	for _, proto := range p.compiled {
		fn, err := runner.loadFunction(proto)
		if err != nil {
			return luaResult{}, err
		}

		result, err := runner.call(fn, p.config)
		if err != nil {
			return luaResult{}, err
		}
		if result.respond {
			return result, nil
		}
	}

	return runner.collect(), nil
}

// compileFunctionCount counts Lua chunk compilations; requests must never
// recompile the static configured functions.
var compileFunctionCount atomic.Int32

// compileFunction parses and compiles a Lua source chunk into an immutable
// prototype shared by every per-request Lua state.
func compileFunction(source string) (*lua.FunctionProto, error) {
	compileFunctionCount.Add(1)
	chunk, err := lua_parse.Parse(strings.NewReader(source), "serverless")
	if err != nil {
		return nil, fmt.Errorf("failed to loadstring: %w", err)
	}
	proto, err := lua.Compile(chunk, "serverless")
	if err != nil {
		return nil, fmt.Errorf("failed to loadstring: %w", err)
	}
	return proto, nil
}

// validateFunctionProto executes the precompiled chunk in a scratch state to
// confirm it evaluates to a Lua function.
func validateFunctionProto(ctx context.Context, proto *lua.FunctionProto) error {
	runner := newLuaRunner(ctx, nil, nil)
	defer runner.close()

	_, err := runner.loadFunction(proto)
	return err
}

func isRequestPhase(phase string) bool {
	switch phase {
	case "", "rewrite", "access", "before_proxy":
		return true
	default:
		return false
	}
}

type luaRunner struct {
	state        *lua.LState
	req          *http.Request
	resp         *base.BufferedResponseWriter
	originalBody string
	sayBody      bytes.Buffer
	luaContext   *lua.LTable
}

func newLuaRunner(ctx context.Context, r *http.Request, resp *base.BufferedResponseWriter) *luaRunner {
	l := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, library := range []struct {
		name string
		open lua.LGFunction
	}{
		{name: lua.LoadLibName, open: lua.OpenPackage},
		{name: lua.BaseLibName, open: lua.OpenBase},
		{name: lua.TabLibName, open: lua.OpenTable},
		{name: lua.StringLibName, open: lua.OpenString},
		{name: lua.MathLibName, open: lua.OpenMath},
		{name: lua.CoroutineLibName, open: lua.OpenCoroutine},
	} {
		l.Push(l.NewFunction(library.open))
		l.Push(lua.LString(library.name))
		l.Call(1, 0)
	}
	l.SetGlobal("dofile", lua.LNil)
	l.SetGlobal("loadfile", lua.LNil)
	packageTable := l.GetGlobal(lua.LoadLibName).(*lua.LTable)
	loaders := l.GetField(packageTable, "loaders").(*lua.LTable)
	preloadOnly := l.NewTable()
	preloadOnly.RawSetInt(1, l.RawGetInt(loaders, 1))
	l.SetField(packageTable, "loaders", preloadOnly)
	l.SetField(l.Get(lua.RegistryIndex), "_LOADERS", preloadOnly)
	if ctx == nil {
		ctx = context.Background()
	}
	l.SetContext(ctx)
	runner := &luaRunner{
		state: l,
		req:   r,
		resp:  resp,
	}
	if resp != nil {
		runner.originalBody = string(resp.Body())
	}

	runner.registerCJSON()
	runner.registerApisixCore()
	runner.registerNgx()
	return runner
}

func (r *luaRunner) close() {
	r.state.Close()
}

func (r *luaRunner) loadFunction(proto *lua.FunctionProto) (lua.LValue, error) {
	r.state.Push(r.state.NewFunctionFromProto(proto))
	if err := r.state.PCall(0, lua.MultRet, nil); err != nil {
		r.state.SetTop(0)
		return lua.LNil, fmt.Errorf("failed to loadstring: %w", err)
	}
	if r.state.GetTop() == 0 {
		return lua.LNil, fmt.Errorf("only accept Lua function, the input code type is nil")
	}

	fn := r.state.Get(-1)
	r.state.Pop(1)
	if fn.Type() != lua.LTFunction {
		return lua.LNil, fmt.Errorf("only accept Lua function, the input code type is %s", fn.Type().String())
	}

	return fn, nil
}

func (r *luaRunner) call(fn lua.LValue, conf Config) (luaResult, error) {
	l := r.state
	l.Push(fn)
	l.Push(r.configTable(conf))
	l.Push(r.ctxTable())
	if err := l.PCall(2, lua.MultRet, nil); err != nil {
		return luaResult{}, err
	}
	r.persistContext()

	code := l.Get(1)
	body := l.Get(2)
	l.SetTop(0)

	if code != lua.LNil || body != lua.LNil {
		result := r.collect()
		result.respond = true
		if status, ok := luaValueToStatus(code); ok {
			result.status = status
		}
		if body != lua.LNil {
			result.body = luaValueToBody(body)
			result.bodyModified = true
		}
		return result, nil
	}

	return r.collect(), nil
}

func (r *luaRunner) configTable(conf Config) lua.LValue {
	l := r.state
	t := l.NewTable()
	t.RawSetString("phase", lua.LString(conf.Phase))
	functions := l.NewTable()
	for i, fn := range conf.Functions {
		functions.RawSetInt(i+1, lua.LString(fn))
	}
	t.RawSetString("functions", functions)
	return t
}

func (r *luaRunner) ctxTable() lua.LValue {
	l := r.state
	t := l.NewTable()
	r.luaContext = t
	if r.req == nil {
		return t
	}

	currReq := l.NewTable()
	currReq.RawSetString("_path", lua.LString(r.req.URL.Path))
	t.RawSetString("curr_req_matched", currReq)

	vars := l.NewTable()
	vars.RawSetString("uri", lua.LString(r.req.URL.Path))
	vars.RawSetString("request_uri", lua.LString(r.req.URL.RequestURI()))
	vars.RawSetString("request_method", lua.LString(r.req.Method))
	vars.RawSetString("host", lua.LString(r.req.Host))
	t.RawSetString("var", vars)
	return t
}

func (r *luaRunner) persistContext() {
	if r.req == nil || r.luaContext == nil {
		return
	}
	externalUser := r.luaContext.RawGetString("external_user")
	if externalUser == lua.LNil {
		return
	}
	apisixctx.RegisterApisixVar(r.req, "$external_user", luautil.LuaValueToGo(externalUser))
}

func (r *luaRunner) registerNgx() {
	l := r.state
	ngx := l.NewTable()
	ngx.RawSetString("ERR", lua.LNumber(3))
	ngx.RawSetString("WARN", lua.LNumber(4))
	ngx.RawSetString("INFO", lua.LNumber(6))
	ngx.RawSetString("log", l.NewFunction(func(l *lua.LState) int {
		return 0
	}))
	ngx.RawSetString("say", l.NewFunction(func(l *lua.LState) int {
		top := l.GetTop()
		for i := 1; i <= top; i++ {
			r.sayBody.WriteString(luaValueToString(l.Get(i)))
		}
		r.sayBody.WriteByte('\n')
		return 0
	}))

	req := l.NewTable()
	req.RawSetString("set_header", l.NewFunction(func(l *lua.LState) int {
		if r.req != nil {
			r.req.Header.Set(l.CheckString(1), luaValueToString(l.Get(2)))
		}
		return 0
	}))
	req.RawSetString("get_headers", l.NewFunction(func(l *lua.LState) int {
		headers := l.NewTable()
		if r.req != nil {
			for field, values := range r.req.Header {
				if len(values) == 1 {
					headers.RawSetString(field, lua.LString(values[0]))
				} else {
					headers.RawSetString(field, stringSliceToLuaTable(l, values))
				}
			}
		}
		l.Push(headers)
		return 1
	}))
	ngx.RawSetString("req", req)

	ngx.RawSetString("header", r.headerTable())
	ngx.RawSetString("arg", r.argTable())
	ngx.RawSetString("status", lua.LNumber(r.status()))
	l.SetGlobal("ngx", ngx)
}

func (r *luaRunner) headerTable() lua.LValue {
	l := r.state
	headers := l.NewTable()
	if r.resp == nil {
		return headers
	}

	for field, values := range r.resp.Header() {
		if len(values) == 1 {
			headers.RawSetString(field, lua.LString(values[0]))
		} else {
			headers.RawSetString(field, stringSliceToLuaTable(l, values))
		}
	}
	return headers
}

func (r *luaRunner) argTable() lua.LValue {
	l := r.state
	arg := l.NewTable()
	if r.resp != nil {
		arg.RawSetInt(1, lua.LString(r.originalBody))
		arg.RawSetInt(2, lua.LBool(true))
	}
	return arg
}

func (r *luaRunner) status() int {
	if r.resp == nil {
		return http.StatusOK
	}
	return r.resp.StatusCode()
}

func (r *luaRunner) registerCJSON() {
	r.state.PreloadModule("cjson", func(l *lua.LState) int {
		mod := l.NewTable()
		mod.RawSetString("decode", l.NewFunction(func(l *lua.LState) int {
			var v any
			if err := json.Unmarshal([]byte(l.CheckString(1)), &v); err != nil {
				l.RaiseError("%s", err.Error())
				return 0
			}
			l.Push(luautil.GoValueToLua(l, v))
			return 1
		}))
		mod.RawSetString("encode", l.NewFunction(func(l *lua.LState) int {
			data, err := json.Marshal(luautil.LuaValueToGo(l.Get(1)))
			if err != nil {
				l.RaiseError("%s", err.Error())
				return 0
			}
			l.Push(lua.LString(data))
			return 1
		}))
		l.Push(mod)
		return 1
	})
}

func (r *luaRunner) registerApisixCore() {
	r.state.PreloadModule("apisix.core", func(l *lua.LState) int {
		mod := l.NewTable()
		response := l.NewTable()
		response.RawSetString("hold_body_chunk", l.NewFunction(func(l *lua.LState) int {
			if r.resp == nil {
				l.Push(lua.LNil)
				return 1
			}
			l.Push(lua.LString(r.originalBody))
			return 1
		}))
		response.RawSetString("clear_header_as_body_modified", l.NewFunction(func(l *lua.LState) int {
			if r.resp != nil {
				r.resp.Header().Del("Content-Length")
			}
			return 0
		}))
		mod.RawSetString("response", response)

		request := l.NewTable()
		request.RawSetString("set_header", l.NewFunction(func(l *lua.LState) int {
			if r.req != nil {
				r.req.Header.Set(l.CheckString(2), luaValueToString(l.Get(3)))
			}
			return 0
		}))
		mod.RawSetString("request", request)

		ctx := l.NewTable()
		ctx.RawSetString("register_var", l.NewFunction(func(l *lua.LState) int {
			return 0
		}))
		mod.RawSetString("ctx", ctx)

		l.Push(mod)
		return 1
	})
}

func (r *luaRunner) collect() luaResult {
	result := luaResult{
		status: r.status(),
		header: http.Header{},
	}

	ngx, _ := r.state.GetGlobal("ngx").(*lua.LTable)
	if ngx == nil {
		return result
	}

	if status, ok := luaValueToStatus(ngx.RawGetString("status")); ok {
		result.status = status
	}
	if header, ok := ngx.RawGetString("header").(*lua.LTable); ok {
		result.header = luautil.LuaTableToHeader(header)
	}
	if arg, ok := ngx.RawGetString("arg").(*lua.LTable); ok {
		body := luaValueToString(arg.RawGetInt(1))
		if r.resp != nil && body != r.originalBody {
			result.body = []byte(body)
			result.bodyModified = true
		}
	}
	if r.sayBody.Len() > 0 {
		result.body = bytes.TrimSuffix(r.sayBody.Bytes(), []byte("\n"))
		result.bodyModified = true
		result.respond = r.resp == nil
	}

	return result
}

type luaResult struct {
	respond      bool
	status       int
	header       http.Header
	body         []byte
	bodyModified bool
}

func (r luaResult) apply(resp *base.BufferedResponseWriter) {
	if r.status != 0 {
		resp.SetStatusCode(r.status)
	}
	for field, values := range r.header {
		resp.Header().Del(field)
		for _, value := range values {
			resp.Header().Add(field, value)
		}
	}
	if r.bodyModified {
		resp.SetBody(r.body)
		resp.Header().Del("Content-Length")
	}
}

func writeResult(w http.ResponseWriter, result luaResult) {
	for field, values := range result.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	status := result.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(result.body)
}

func luaValueToStatus(value lua.LValue) (int, bool) {
	switch v := value.(type) {
	case lua.LNumber:
		status := float64(v)
		if math.IsNaN(status) || status < 200 || status > 599 || status != math.Trunc(status) {
			return 0, false
		}
		return int(status), true
	case lua.LString:
		status, err := strconv.Atoi(string(v))
		if err != nil || status < 200 || status > 599 {
			return 0, false
		}
		return status, true
	default:
		return 0, false
	}
}

func luaValueToBody(value lua.LValue) []byte {
	if value.Type() == lua.LTTable {
		data, err := json.Marshal(luautil.LuaValueToGo(value))
		if err == nil {
			return data
		}
	}
	return []byte(luaValueToString(value))
}

func luaValueToString(value lua.LValue) string {
	switch v := value.(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return strconv.FormatFloat(float64(v), 'f', -1, 64)
	case lua.LBool:
		if bool(v) {
			return "true"
		}
		return "false"
	case *lua.LNilType:
		return ""
	default:
		return value.String()
	}
}

func stringSliceToLuaTable(l *lua.LState, values []string) *lua.LTable {
	t := l.NewTable()
	for i, value := range values {
		t.RawSetInt(i+1, lua.LString(value))
	}
	return t
}
