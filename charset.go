package main

import (
	"github.com/yuin/charsetutil"
	"github.com/yuin/gopher-lua"
)

var charsetMod map[string]lua.LGFunction = map[string]lua.LGFunction{
	"decode": func(L *lua.LState) int {
		bytes, err := charsetutil.DecodeString(L.CheckString(1), L.CheckString(2))
		if err != nil {
			L.RaiseError(err.Error())
		}
		L.Push(lua.LString(string(bytes)))
		return 1
	},
	"encode": func(L *lua.LState) int {
		s, err := charsetutil.EncodeString(L.CheckString(1), L.CheckString(2))
		if err != nil {
			L.RaiseError(err.Error())
		}
		L.Push(lua.LString(s))
		return 1
	},
}
