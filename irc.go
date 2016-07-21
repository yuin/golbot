package main

import (
	"log"
	"reflect"
	"time"

	"github.com/layeh/gopher-luar"
	"github.com/thoj/go-ircevent"
	"github.com/yuin/gopher-lua"
)

type ircChatClient struct {
	ircobj *irc.Connection
}

func (client *ircChatClient) Logger() *log.Logger {
	return client.ircobj.Log
}

func newIRCBot(L *lua.LState, nick, user string, logger *log.Logger, opt *lua.LTable) chatClient {
	ircobj := irc.IRC(nick, user)
	if logger != nil {
		ircobj.Log = logger
	}
	ircobj.UseTLS = lua.LVAsBool(L.GetField(opt, "useTLS"))
	if s, ok := getStringField(L, opt, "password"); ok {
		ircobj.Password = s
	}
	L.Push(luar.New(L, ircobj))
	return &ircChatClient{ircobj}
}

func newIRCLuaState(L *lua.LState, conf string, nworker int, startHttpServer func()) {
	proxyLuar(L, irc.Connection{}, func(L *lua.LState, key string) bool {
		switch key {
		case "on":
			L.Push(L.NewFunction(func(L *lua.LState) int {
				self := L.CheckUserData(1).Value.(*irc.Connection)
				fn := L.CheckFunction(3)
				self.AddCallback(L.CheckString(2), func(e *irc.Event) {
					mutex.Lock()
					defer mutex.Unlock()
					pushN(L, fn, luar.New(L, e))
					L.PCall(1, 0, nil)
				})
				return 0
			}))
		case "serve":
			L.Push(L.NewFunction(func(L *lua.LState) int {
				irc := L.CheckUserData(1).Value.(*irc.Connection)
				v := reflect.ValueOf(*irc)
				for i := 0; i < nworker; i++ {
					irc.Log.Printf("spawn worker\n")
					go func() {
						L := newLuaState(conf)
						for !v.FieldByName("quit").Bool() {
							pushN(L, L.GetGlobal("worker"), <-luaWorkerChan)
							L.PCall(1, 0, nil)
						}
					}()
				}
				startHttpServer()

				fn := L.OptFunction(2, L.NewFunction(func(L *lua.LState) int { return 0 }))
				errChan := irc.ErrorChan()
				for !v.FieldByName("quit").Bool() {
					select {
					case err := <-errChan:
						irc.Log.Printf("Error, disconnected: %s\n", err)
						for !v.FieldByName("quit").Bool() {
							if err = irc.Reconnect(); err != nil {
								irc.Log.Printf("Error while reconnecting: %s\n", err)
								time.Sleep(60 * time.Second)
							} else {
								errChan = irc.ErrorChan()
								break
							}
						}
					case msg := <-luaMainChan:
						func() {
							mutex.Lock()
							defer mutex.Unlock()
							pushN(L, fn, msg)
							L.PCall(1, 0, nil)
						}()
					}
				}
				return 0
			}))
		default:
			return false

		}
		return true
	})

	proxyLuar(L, irc.Event{}, nullProxy)
}
