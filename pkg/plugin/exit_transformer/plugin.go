package exit_transformer

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/luautil"
	lua "github.com/yuin/gopher-lua"
	lua_parse "github.com/yuin/gopher-lua/parse"
)

type Plugin struct {
	base.BasePlugin
	config Config

	// compiled holds the immutable precompiled function chunks, parsed once
	// in PostInit instead of on every response.
	compiled []*lua.FunctionProto
}

const (
	priority = 22950
	name     = "exit-transformer"
)

const schema = `
{
  "type": "object",
  "properties": {
    "functions": {
      "type": "array",
      "items": {
        "type": "string"
      }
    }
  },
  "required": ["functions"]
}
`

type Config struct {
	Functions []string `json:"functions"`
}

type exitResponse struct {
	status       int
	body         []byte
	header       http.Header
	method       string
	bodyReplaced bool
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	runner := newTransformerRunner(nil)
	defer runner.close()

	for _, source := range p.config.Functions {
		proto, err := compileFunction(source)
		if err != nil {
			return err
		}
		if err := runner.validate(proto); err != nil {
			return err
		}
		p.compiled = append(p.compiled, proto)
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// RunBufferedBodyFilter applies each configured Lua transformer to one
// canonical response state. A failed transformer keeps the prior state and is
// logged, matching the legacy best-effort payload behavior.
func (p *Plugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || !p.AppliesToResponseSource(responseSource(r)) {
		return nil
	}
	resp := exitResponse{
		status: state.Status,
		body:   append([]byte(nil), state.Body...),
		header: state.Header.Clone(),
		method: r.Method,
	}
	runner := newTransformerRunner(r)
	defer runner.close()
	for _, proto := range p.compiled {
		transformed, err := runner.transform(resp, proto)
		if err != nil {
			logger.Errorf("exit-transformer: %v", err)
			continue
		}
		resp = transformed
	}
	state.Status = resp.status
	state.Body = append([]byte(nil), resp.body...)
	state.Header = resp.header.Clone()
	return nil
}

func (p *Plugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	return source == apisixctx.ResponseSourceAPISIX || source == apisixctx.ResponseSourceEarlyStop
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			next.ServeHTTP(w, r)
			return
		}
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		resp := exitResponse{
			status: recorder.StatusCode(),
			body:   recorder.Body(),
			header: recorder.Header().Clone(),
			method: r.Method,
		}
		if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source == "upstream" {
			writeResponse(w, resp)
			return
		}
		runner := newTransformerRunner(r)
		defer runner.close()
		for _, proto := range p.compiled {
			transformed, err := runner.transform(resp, proto)
			if err != nil {
				logger.Errorf("exit-transformer: %v", err)
				continue
			}
			resp = transformed
		}
		writeResponse(w, resp)
	})
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

func writeResponse(w http.ResponseWriter, resp exitResponse) {
	for field, values := range resp.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.status)
	if brw, ok := w.(*base.BufferedResponseWriter); ok {
		if !base.ResponseAllowsBody(resp.method, resp.status) {
			return
		}
		if resp.bodyReplaced {
			brw.ReplaceBody(resp.body)
		} else {
			brw.SetBody(resp.body)
		}
		return
	}
	if base.ResponseAllowsBody(resp.method, resp.status) {
		_, _ = w.Write(resp.body)
	}
}

// transformerRunner executes a configured Lua function per response. Only
// base/table/string/math libraries are opened; os, io, debug, package.loadlib
// and filesystem or process APIs are intentionally unavailable.
type transformerRunner struct {
	state *lua.LState
	req   *http.Request
}

func newTransformerRunner(r *http.Request) *transformerRunner {
	state := lua.NewState(lua.Options{SkipOpenLibs: true})
	runner := &transformerRunner{state: state, req: r}
	runner.openSafeLibraries()
	runner.preloadApisixCore()
	return runner
}

func (r *transformerRunner) close() {
	r.state.Close()
}

func (r *transformerRunner) openSafeLibraries() {
	lua.OpenBase(r.state)
	lua.OpenTable(r.state)
	lua.OpenString(r.state)
	lua.OpenMath(r.state)
	r.state.SetTop(0)
	packagemod := r.state.NewTable()
	packagemod.RawSetString("preload", r.state.NewTable())
	packagemod.RawSetString("loaded", r.state.NewTable())
	r.state.SetGlobal("package", packagemod)
	r.state.SetGlobal("require", r.state.NewFunction(func(l *lua.LState) int {
		name := l.CheckString(1)
		loaded := l.GetField(l.GetGlobal("package"), "loaded").(*lua.LTable)
		if mod := loaded.RawGetString(name); mod != lua.LNil {
			l.Push(mod)
			return 1
		}
		preload := l.GetField(l.GetGlobal("package"), "preload").(*lua.LTable)
		loader := preload.RawGetString(name)
		if loader == lua.LNil {
			l.RaiseError("module '%s' not found", name)
			return 0
		}
		l.Push(loader)
		l.Push(lua.LString(name))
		if err := l.PCall(1, 1, nil); err != nil {
			l.RaiseError("%v", err)
			return 0
		}
		mod := l.Get(l.GetTop())
		l.SetTop(0)
		loaded.RawSetString(name, mod)
		l.Push(mod)
		return 1
	}))
}

func (r *transformerRunner) preloadApisixCore() {
	r.state.PreloadModule("apisix.core", func(l *lua.LState) int {
		mod := l.NewTable()

		request := l.NewTable()
		request.RawSetString("headers", l.NewFunction(func(l *lua.LState) int {
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
		mod.RawSetString("request", request)

		log := l.NewTable()
		log.RawSetString("warn", l.NewFunction(func(l *lua.LState) int {
			logger.Warnf("%s", luaValueToString(l.Get(1)))
			return 0
		}))
		mod.RawSetString("log", log)

		l.Push(mod)
		return 1
	})
}

func (r *transformerRunner) validate(proto *lua.FunctionProto) error {
	r.state.Push(r.state.NewFunctionFromProto(proto))
	if err := r.state.PCall(0, lua.MultRet, nil); err != nil {
		r.state.SetTop(0)
		return fmt.Errorf("unexpected symbol: %w", err)
	}
	result := r.state.Get(1)
	r.state.SetTop(0)
	if result != lua.LNil && result.Type() != lua.LTFunction {
		return fmt.Errorf("only accept Lua function, the input code type is %s", result.Type().String())
	}
	return nil
}

// compileFunctionCount counts Lua chunk compilations; responses must never
// recompile the static configured functions.
var compileFunctionCount atomic.Int32

// compileFunction parses and compiles a Lua source chunk into an immutable
// prototype shared by every per-request Lua state.
func compileFunction(source string) (*lua.FunctionProto, error) {
	compileFunctionCount.Add(1)
	chunk, err := lua_parse.Parse(strings.NewReader(source), "exit-transformer")
	if err != nil {
		return nil, fmt.Errorf("unexpected symbol: %w", err)
	}
	proto, err := lua.Compile(chunk, "exit-transformer")
	if err != nil {
		return nil, fmt.Errorf("unexpected symbol: %w", err)
	}
	return proto, nil
}

func (r *transformerRunner) transform(resp exitResponse, proto *lua.FunctionProto) (exitResponse, error) {
	fn := r.state.NewFunctionFromProto(proto)
	code, body, header := responseValues(r.state, resp)
	r.state.Push(fn)
	r.state.Push(code)
	r.state.Push(body)
	r.state.Push(header)
	if err := r.state.PCall(3, lua.MultRet, nil); err != nil {
		r.state.SetTop(0)
		return resp, err
	}
	if fnResult := r.state.Get(1); fnResult.Type() == lua.LTFunction {
		r.state.SetTop(0)
		r.state.Push(fnResult)
		r.state.Push(code)
		r.state.Push(body)
		r.state.Push(header)
		if err := r.state.PCall(3, lua.MultRet, nil); err != nil {
			r.state.SetTop(0)
			return resp, err
		}
	}

	transformed := exitResponse{
		status:       resp.status,
		body:         resp.body,
		header:       resp.header,
		method:       resp.method,
		bodyReplaced: resp.bodyReplaced,
	}
	if status, ok := luaValueToStatus(r.state.Get(1)); ok && status > 0 {
		transformed.status = status
	}
	if body := r.state.Get(2); body != lua.LNil {
		transformed.body = luaValueToBody(body)
		transformed.bodyReplaced = true
	}
	if value := r.state.Get(3); value != lua.LNil {
		if table, ok := value.(*lua.LTable); ok {
			transformed.header = luautil.LuaTableToHeader(table)
		}
	}
	if transformed.bodyReplaced {
		base.InvalidateBodyDerivedHeaders(transformed.header)
	}
	r.state.SetTop(0)
	return transformed, nil
}

func responseValues(l *lua.LState, resp exitResponse) (lua.LValue, lua.LValue, lua.LValue) {
	header := l.NewTable()
	for field, values := range resp.header {
		if len(values) == 1 {
			header.RawSetString(field, lua.LString(values[0]))
		} else {
			header.RawSetString(field, stringSliceToLuaTable(l, values))
		}
	}
	var bodyValue any
	if json.Unmarshal(resp.body, &bodyValue) == nil {
		return lua.LNumber(resp.status), luautil.GoValueToLua(l, bodyValue), header
	}
	return lua.LNumber(resp.status), lua.LString(string(resp.body)), header
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
