package main

import (
	"log"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/layeh/gopher-luar"
	"github.com/thoj/go-ircevent"
	"github.com/yuin/gopher-lua"
)

var ircChatClientTypeName = "ircChatClient"

type ircChatClient struct {
	ircobj       *irc.Connection
	commonOption *CommonClientOption
	conn         string
}

func (client *ircChatClient) Logger() *log.Logger {
	return client.ircobj.Log
}

func (client *ircChatClient) CommonOption() *CommonClientOption {
	return client.commonOption
}

func (client *ircChatClient) Say(target, message string) {
	client.ircobj.Privmsg(target, message)
}

func (client *ircChatClient) On(L *lua.LState, action string, fn *lua.LFunction) {
	client.ircobj.AddCallback(action, func(e *irc.Event) {
		mutex.Lock()
		defer mutex.Unlock()
		pushN(L, fn, luar.New(L, e))
		L.PCall(1, 0, nil)
	})
}

func (client *ircChatClient) Respond(L *lua.LState, pattern string, fn *lua.LFunction) {
	re := regexp.MustCompile(pattern)
	client.ircobj.AddCallback("PRIVMSG", func(e *irc.Event) {
		mutex.Lock()
		defer mutex.Unlock()
		matches := re.FindAllStringSubmatch(e.Message(), -1)
		if len(matches) != 0 {
			pushN(L, fn, luar.New(L, matches[0]), luar.New(L, NewMessageEvent(e.Nick, e.Arguments[0], e.Message(), e)))
			if err := L.PCall(2, 0, nil); err != nil {
				client.ircobj.Log.Printf("[ERROR] %s", err.Error())
			}
		}
	})
}

func (client *ircChatClient) Serve(L *lua.LState, fn *lua.LFunction) {
	irc := client.ircobj

	if len(irc.Server) == 0 {
		parts := strings.Split(client.conn, ",")
		if err := irc.Connect(parts[0]); err != nil {
			L.RaiseError(err.Error())
		}
		for i := 1; i < len(parts); i++ {
			irc.Join(parts[i])
		}
	}

	v := reflect.ValueOf(*irc)
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
}

func registerIRCChatClientType(L *lua.LState) {
	registerChatClientType(L, ircChatClientTypeName)
	proxyLuar(L, irc.Connection{}, nil)
	proxyLuar(L, irc.Event{}, nil)
}

func newIRCChatClient(L *lua.LState, co *CommonClientOption, opt *lua.LTable) {
	nickname, nok := getStringField(L, opt, "nickname")
	username, uok := getStringField(L, opt, "username")
	if !nok || !uok {
		L.RaiseError("'nickname' and 'username' are required")
	}

	ircobj := irc.IRC(nickname, username)
	chatClient := &ircChatClient{ircobj, co, "127.0.0.1:6667"}

	if co.Logger != nil {
		ircobj.Log = co.Logger
	}
	ircobj.UseTLS = lua.LVAsBool(L.GetField(opt, "useTLS"))
	if s, ok := getStringField(L, opt, "password"); ok {
		ircobj.Password = s
	}
	if s, ok := getStringField(L, opt, "conn"); ok {
		chatClient.conn = s
	}
	L.Push(newChatClient(L, ircChatClientTypeName, chatClient, luar.New(L, ircobj).(*lua.LUserData)))
}
