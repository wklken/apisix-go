package serverless

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	lua "github.com/yuin/gopher-lua"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	preFunctionName      = "serverless-pre-function"
	preFunctionPriority  = 10000
	postFunctionName     = "serverless-post-function"
	postFunctionPriority = -2000
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
	if p.config.Phase == "" {
		p.config.Phase = "access"
	}
	for _, fn := range p.config.Functions {
		if err := validateFunction(fn); err != nil {
			return err
		}
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isRequestPhase(p.config.Phase) {
			result, err := p.runFunctions(r, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if result.respond {
				writeResult(w, result)
				return
			}

			next.ServeHTTP(w, r)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)

		result, err := p.runFunctions(r, recorder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result.apply(recorder)
		recorder.writeTo(w)
	})
}

func (p *Plugin) runFunctions(r *http.Request, resp *responseRecorder) (luaResult, error) {
	runner := newLuaRunner(r, resp)
	defer runner.close()

	for _, source := range p.config.Functions {
		fn, err := runner.loadFunction(source)
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

func validateFunction(source string) error {
	runner := newLuaRunner(nil, nil)
	defer runner.close()

	_, err := runner.loadFunction(source)
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
	resp         *responseRecorder
	originalBody string
	sayBody      bytes.Buffer
}

func newLuaRunner(r *http.Request, resp *responseRecorder) *luaRunner {
	l := lua.NewState()
	runner := &luaRunner{
		state: l,
		req:   r,
		resp:  resp,
	}
	if resp != nil {
		runner.originalBody = string(resp.body.Bytes())
	}

	runner.registerCJSON()
	runner.registerApisixCore()
	runner.registerNgx()
	return runner
}

func (r *luaRunner) close() {
	r.state.Close()
}

func (r *luaRunner) loadFunction(source string) (lua.LValue, error) {
	if err := r.state.DoString(source); err != nil {
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

	for field, values := range r.resp.header {
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
	return r.resp.statusCode
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
			l.Push(goValueToLua(l, v))
			return 1
		}))
		mod.RawSetString("encode", l.NewFunction(func(l *lua.LState) int {
			data, err := json.Marshal(luaValueToGo(l.Get(1)))
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
				r.resp.header.Del("Content-Length")
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
		result.header = luaTableToHeader(header)
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

func (r luaResult) apply(resp *responseRecorder) {
	if r.status != 0 {
		resp.statusCode = r.status
	}
	for field, values := range r.header {
		resp.header.Del(field)
		for _, value := range values {
			resp.header.Add(field, value)
		}
	}
	if r.bodyModified {
		resp.body.Reset()
		resp.body.Write(r.body)
		resp.header.Del("Content-Length")
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
	w.Write(result.body)
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	w.Write(r.body.Bytes())
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
