package luautil

import (
	"reflect"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

func TestLuaTableToHeader(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	tbl := l.NewTable()
	tbl.RawSetString("X-Field", lua.LString("single"))
	list := l.NewTable()
	list.RawSetInt(1, lua.LString("a"))
	list.RawSetInt(2, lua.LString("b"))
	tbl.RawSetString("X-List", list)
	tbl.RawSetString("X-Number", lua.LNumber(42))

	header := LuaTableToHeader(tbl)
	if got := header.Values("X-Field"); !reflect.DeepEqual(got, []string{"single"}) {
		t.Fatalf("X-Field = %v", got)
	}
	if got := header.Values("X-List"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("X-List = %v", got)
	}
	if got := header.Get("X-Number"); got != "42" {
		t.Fatalf("X-Number = %q", got)
	}
}

func TestGoValueToLuaRoundTrip(t *testing.T) {
	l := lua.NewState()
	defer l.Close()

	value := map[string]any{
		"str":    "text",
		"num":    float64(3.5),
		"bool":   true,
		"list":   []any{float64(1), "two", false},
		"nested": map[string]any{"key": "value"},
		"other":  int(7),
	}
	converted := GoValueToLua(l, value)
	back := LuaValueToGo(converted)
	if !reflect.DeepEqual(back, map[string]any{
		"str":    "text",
		"num":    float64(3.5),
		"bool":   true,
		"list":   []any{float64(1), "two", false},
		"nested": map[string]any{"key": "value"},
		"other":  "7",
	}) {
		t.Fatalf("round trip = %#v", back)
	}
}

func TestLuaTableToGoArrayAndObject(t *testing.T) {
	l := lua.NewState()
	defer l.Close()

	array := l.NewTable()
	array.RawSetInt(1, lua.LString("a"))
	array.RawSetInt(2, lua.LString("b"))
	if got := LuaTableToGo(array); !reflect.DeepEqual(got, []any{"a", "b"}) {
		t.Fatalf("array table = %#v", got)
	}

	obj := l.NewTable()
	obj.RawSetString("k", lua.LNumber(1))
	obj.RawSetInt(1, lua.LString("mixed"))
	got := LuaTableToGo(obj).(map[string]any)
	if got["k"] != float64(1) {
		t.Fatalf("object key k = %v", got["k"])
	}
	if got["1"] != "mixed" {
		t.Fatalf("numeric key = %v", got["1"])
	}
}

func TestLuaValueToGoPrimitives(t *testing.T) {
	tests := []struct {
		name  string
		value lua.LValue
		want  any
	}{
		{name: "string", value: lua.LString("s"), want: "s"},
		{name: "number", value: lua.LNumber(2.5), want: float64(2.5)},
		{name: "bool", value: lua.LBool(true), want: true},
		{name: "nil", value: lua.LNil, want: nil},
		{name: "function falls back to string", value: lua.LBool(false), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LuaValueToGo(tt.value); got != tt.want {
				t.Fatalf("LuaValueToGo() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLuaTableToHeaderSkipsEmptyFields(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	tbl := l.NewTable()
	tbl.RawSet(lua.LNil, lua.LString("x"))
	tbl.RawSetString("Valid", lua.LString("y"))

	header := LuaTableToHeader(tbl)
	if len(header) != 1 || header.Get("Valid") != "y" {
		t.Fatalf("header = %v, want only Valid", header)
	}
}
