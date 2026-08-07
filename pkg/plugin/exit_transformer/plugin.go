package exit_transformer

import (
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
	status int
	body   []byte
	header http.Header
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		resp := exitResponse{
			status: recorder.StatusCode(),
			body:   recorder.Body(),
			header: recorder.Header().Clone(),
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

func writeResponse(w http.ResponseWriter, resp exitResponse) {
	for field, values := range resp.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.status)
	if brw, ok := w.(*base.BufferedResponseWriter); ok {
		brw.SetBody(resp.body)
	} else {
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
		status: resp.status,
		body:   resp.body,
		header: resp.header,
	}
	if status, ok := luaValueToStatus(r.state.Get(1)); ok && status > 0 {
		transformed.status = status
	}
	if body := r.state.Get(2); body != lua.LNil {
		transformed.body = luaValueToBody(body)
		transformed.header.Set("Content-Length", fmt.Sprint(len(transformed.body)))
	}
	if value := r.state.Get(3); value != lua.LNil {
		if table, ok := value.(*lua.LTable); ok {
			transformed.header = luaTableToHeader(table)
		}
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
		return lua.LNumber(resp.status), goValueToLua(l, bodyValue), header
	}
	return lua.LNumber(resp.status), lua.LString(string(resp.body)), header
}

func luaValueToStatus(value lua.LValue) (int, bool) {
	switch v := value.(type) {
	case lua.LNumber:
		return int(v), true
	case lua.LString:
		status, err := strconv.Atoi(string(v))
		return status, err == nil
	default:
		return 0, false
	}
}

func luaValueToBody(value lua.LValue) []byte {
	if value.Type() == lua.LTTable {
		data, err := json.Marshal(luaValueToGo(value))
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

func luaTableToHeader(t *lua.LTable) http.Header {
	header := http.Header{}
	t.ForEach(func(key lua.LValue, value lua.LValue) {
		field := luaValueToString(key)
		if field == "" {
			return
		}
		if value.Type() == lua.LTTable {
			value.(*lua.LTable).ForEach(func(_ lua.LValue, item lua.LValue) {
				header.Add(field, luaValueToString(item))
			})
			return
		}
		header.Set(field, luaValueToString(value))
	})
	return header
}

func goValueToLua(l *lua.LState, value any) lua.LValue {
	switch v := value.(type) {
	case nil:
		return lua.LNil
	case map[string]any:
		t := l.NewTable()
		for key, item := range v {
			t.RawSetString(key, goValueToLua(l, item))
		}
		return t
	case []any:
		t := l.NewTable()
		for i, item := range v {
			t.RawSetInt(i+1, goValueToLua(l, item))
		}
		return t
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case bool:
		return lua.LBool(v)
	default:
		return lua.LString(fmt.Sprint(v))
	}
}

func luaValueToGo(value lua.LValue) any {
	switch v := value.(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return float64(v)
	case lua.LBool:
		return bool(v)
	case *lua.LTable:
		return luaTableToGo(v)
	case *lua.LNilType:
		return nil
	default:
		return value.String()
	}
}

func luaTableToGo(t *lua.LTable) any {
	if isArrayTable(t) {
		values := make([]any, 0, t.Len())
		for i := 1; i <= t.Len(); i++ {
			values = append(values, luaValueToGo(t.RawGetInt(i)))
		}
		return values
	}

	values := map[string]any{}
	t.ForEach(func(key lua.LValue, value lua.LValue) {
		values[luaValueToString(key)] = luaValueToGo(value)
	})
	return values
}

func isArrayTable(t *lua.LTable) bool {
	if t.Len() == 0 {
		return false
	}
	array := true
	t.ForEach(func(key lua.LValue, _ lua.LValue) {
		if _, ok := key.(lua.LNumber); !ok {
			array = false
		}
	})
	return array
}
