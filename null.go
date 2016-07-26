package main

import (
	"log"
	"os"
	"regexp"

	"github.com/yuin/gopher-lua"
)

const nullChatClientTypeName = "nullChatClient"
const nullDefaultConfigLua = `
  local bot = golbot.newbot("Null", {
    http = "0.0.0.0:6669",
    log = {"seelog", type="adaptive", mininterval="200000000", maxinterval="1000000000", critmsgcount="5",
      {"formats",
        {"format", id="main", format="%Date(2006-01-02 15:04:05) [%Level] %Msg"},
      },
      {"outputs", formatid="main",
        {"filter", levels="trace,debug,info,warn,error,critical",
          {"console"}
        },
      }
    }
  })
`

type nullChatClient struct {
	commonOption *CommonClientOption
}

func (client *nullChatClient) Logger() *log.Logger {
	return client.commonOption.Logger
}

func (client *nullChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *nullChatClient) Say(target, message string) {
}

func (client *nullChatClient) On(L *lua.LState, action string, fn *lua.LFunction) {
}

func (client *nullChatClient) Respond(L *lua.LState, pattern *regexp.Regexp, fn *lua.LFunction) {
}

func (client *nullChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	for {
		select {
		case msg := <-luaMainChan:
			func() {
				mutex.Lock()
				defer mutex.Unlock()
				pushN(L, fn, msg)
				L.PCall(1, 0, nil)
			}()
		}
	}
}

func registerNullChatClientType(L *lua.LState) {
	registerChatClientType(L, nullChatClientTypeName)
}

func newNullChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	if co.Logger == nil {
		co.Logger = log.New(os.Stdout, "", log.Lshortfile|log.LstdFlags)
	}
	chatClient := &nullChatClient{co}
	ud := L.NewUserData()
	L.Push(newChatClient(L, nullChatClientTypeName, chatClient, ud))
}
