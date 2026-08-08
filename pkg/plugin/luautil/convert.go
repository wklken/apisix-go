// Package luautil provides shared conversions between Lua values and Go
// values for the serverless and exit-transformer plugins.
package luautil

import (
	"fmt"
	"net/http"
	"strconv"

	lua "github.com/yuin/gopher-lua"
)

// LuaTableToHeader converts a Lua table into a response header map.
func LuaTableToHeader(t *lua.LTable) http.Header {
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

// GoValueToLua converts a Go value into a Lua value.
func GoValueToLua(l *lua.LState, value any) lua.LValue {
	switch v := value.(type) {
	case nil:
		return lua.LNil
	case map[string]any:
		t := l.NewTable()
		for key, item := range v {
			t.RawSetString(key, GoValueToLua(l, item))
		}
		return t
	case []any:
		t := l.NewTable()
		for i, item := range v {
			t.RawSetInt(i+1, GoValueToLua(l, item))
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

// LuaValueToGo converts a Lua value into a Go value.
func LuaValueToGo(value lua.LValue) any {
	switch v := value.(type) {
	case lua.LString:
		return string(v)
	case lua.LNumber:
		return float64(v)
	case lua.LBool:
		return bool(v)
	case *lua.LTable:
		return LuaTableToGo(v)
	case *lua.LNilType:
		return nil
	default:
		return value.String()
	}
}

// LuaTableToGo converts a Lua table into a Go slice or map.
func LuaTableToGo(t *lua.LTable) any {
	if isArrayTable(t) {
		values := make([]any, 0, t.Len())
		for i := 1; i <= t.Len(); i++ {
			values = append(values, LuaValueToGo(t.RawGetInt(i)))
		}
		return values
	}

	values := map[string]any{}
	t.ForEach(func(key lua.LValue, value lua.LValue) {
		values[luaValueToString(key)] = LuaValueToGo(value)
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
